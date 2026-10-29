package storage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"godb/catalog"
	"godb/types"
)

func newTestLob(t *testing.T) *lobFile {
	t.Helper()
	l := newLobFile(filepath.Join(t.TempDir(), "t1.lob"))
	t.Cleanup(func() { _ = l.close() })
	return l
}

// 追加写 + 按指针读回；每个 blob 自带长度和 CRC。
func TestLobAppendRead(t *testing.T) {
	l := newTestLob(t)
	var offs []int64
	for i, s := range []string{"a", "hello world", strings.Repeat("x", 100000)} {
		off, err := l.append([]byte(s))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		offs = append(offs, off)
	}
	// 偏移必须是递增的（纯追加，不回填空洞）
	for i := 1; i < len(offs); i++ {
		if offs[i] <= offs[i-1] {
			t.Fatalf("偏移没有递增：%v", offs)
		}
	}
	if got := l.size(); got != 1+12+11+12+100000+12 {
		t.Fatalf("文件大小 %d，期望 %d", got, 1+12+11+12+100000+12)
	}
	// 每条都能原样读回
	want := []string{"a", "hello world", strings.Repeat("x", 100000)}
	for i, off := range offs {
		got, err := l.read(off, len(want[i]))
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if string(got) != want[i] {
			t.Fatalf("第 %d 条读回来不对（%d 字节）", i, len(got))
		}
	}
	// 指针里的长度和文件里的对不上 → 报错，而不是把别人的数据当自己的
	if _, err := l.read(offs[1], 5); err == nil {
		t.Fatal("长度不符应该报错")
	}
}

// 正文被改坏：CRC 必须拦下来（页 CRC 管不到大字段正文，这里自己管）。
func TestLobDetectsCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t1.lob")
	l := newLobFile(p)
	off, err := l.append([]byte("0123456789"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := l.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 改坏正文里一个字节
	f, err := os.OpenFile(p, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("X"), off+lobHdrSize+3); err != nil {
		t.Fatal(err)
	}
	f.Close()

	l2 := newLobFile(p)
	defer l2.close()
	if _, err := l2.read(off, 10); err == nil || !strings.Contains(err.Error(), "校验和") {
		t.Fatalf("期望校验和报错，实际 %v", err)
	}
	// 偏移指错地方：魔数不对，也要报错
	if _, err := l2.read(off+1, 9); err == nil {
		t.Fatal("偏移错位应该报错")
	}
}

// 大文本在行里只留指针：元组从 5000 字节缩到十几字节。
func TestTupleOutlinesBigText(t *testing.T) {
	tp := newTestPages(t)
	tbl := &catalog.Table{
		Name:    "t",
		Columns: []*catalog.Column{{Name: "body", TypeName: "text", Kind: int(types.KText)}},
	}
	big := strings.Repeat("q", 5000)
	tuple, err := tp.encodeRowTuple(types.Row{types.Text(big)})
	if err != nil {
		t.Fatalf("encodeRowTuple: %v", err)
	}
	if len(tuple) > 32 {
		t.Fatalf("5000 字节的行编码成 %d 字节，没有外置", len(tuple))
	}
	if tuple[4] != tagLOB {
		t.Fatalf("行里应该是 LOB 指针，tag = %d", tuple[4])
	}
	row, rep, err := tp.decodeRowTuple(tuple, tbl)
	if err != nil {
		t.Fatalf("decodeRowTuple: %v", err)
	}
	if rep != 0 {
		t.Fatalf("不该有修补，rep = %d", rep)
	}
	if row[0].K != types.KText || row[0].S != big {
		t.Fatalf("读回来不是原文（%d 字节）", len(row[0].S))
	}

	// 小文本仍然内联（不该为 3 个字节跑一趟文件）
	small, err := tp.encodeRowTuple(types.Row{types.Text("abc")})
	if err != nil {
		t.Fatalf("encodeRowTuple: %v", err)
	}
	if small[4] != tagText {
		t.Fatalf("小文本应该内联，tag = %d", small[4])
	}
}

// 一行里有好几个中等文本：加起来超过半页，也要能存（按长度从大到小外置）。
func TestTupleOutlinesUntilItFits(t *testing.T) {
	tp := newTestPages(t)
	tbl := &catalog.Table{
		Name: "t",
		Columns: []*catalog.Column{
			{Name: "a", TypeName: "text", Kind: int(types.KText)},
			{Name: "b", TypeName: "text", Kind: int(types.KText)},
			{Name: "c", TypeName: "text", Kind: int(types.KText)},
			{Name: "d", TypeName: "text", Kind: int(types.KText)},
		},
	}
	// 每列 1200 字节：全内联 4800 字节 > MaxTupleSize(4032)，必须外置一两列
	row := types.Row{
		types.Text(strings.Repeat("a", 1200)),
		types.Text(strings.Repeat("b", 1200)),
		types.Text(strings.Repeat("c", 1200)),
		types.Text(strings.Repeat("d", 1200)),
	}
	tuple, err := tp.encodeRowTuple(row)
	if err != nil {
		t.Fatalf("encodeRowTuple: %v", err)
	}
	if len(tuple) > MaxTupleSize {
		t.Fatalf("元组 %d 字节，还是超过上限 %d", len(tuple), MaxTupleSize)
	}
	got, _, err := tp.decodeRowTuple(tuple, tbl)
	if err != nil {
		t.Fatalf("decodeRowTuple: %v", err)
	}
	for i := range row {
		if got[i].S != row[i].S {
			t.Fatalf("第 %d 列读回来不对", i)
		}
	}
}

// 外置完文本还是放不下（比如一列超长 decimal）：报错，别写坏页。
func TestTupleRejectsWhenStillTooLarge(t *testing.T) {
	tp := newTestPages(t)
	row := types.Row{
		types.Decimal(strings.Repeat("9", MaxTupleSize)),
	}
	if _, err := tp.encodeRowTuple(row); err == nil {
		t.Fatal("超长 decimal 应该报错")
	}
}

// 指针的字节布局别乱动（tag + u64 偏移 + u32 长度）。
func TestLobRefLayout(t *testing.T) {
	tp := newTestPages(t)
	off, err := tp.lob.append([]byte("zz"))
	if err != nil {
		t.Fatal(err)
	}
	ref := make([]byte, 0, lobRefSize)
	ref = append(ref, tagLOB)
	ref = binary.LittleEndian.AppendUint64(ref, uint64(off))
	ref = binary.LittleEndian.AppendUint32(ref, 2)
	if len(ref) != lobRefSize {
		t.Fatalf("指针 %d 字节，常量说的是 %d", len(ref), lobRefSize)
	}
	v, next, err := tp.readLobRef(ref, 0)
	if err != nil {
		t.Fatalf("readLobRef: %v", err)
	}
	if v.S != "zz" || next != lobRefSize {
		t.Fatalf("读回来 %q（next=%d）", v.S, next)
	}
}
