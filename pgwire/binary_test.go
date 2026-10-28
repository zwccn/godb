package pgwire

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"
)

// 这一组用例覆盖「扩展协议 + 二进制结果集」的兼容性回归。
// 背景：Npgsql（及其上的 EFCore / Dapper）默认请求二进制结果，并靠
// RowDescription 里的 format code 决定用哪个类型处理器。这两点以前都没做，
// 表现为 “Unknown message code: 76”、GetInt32 报 <unknown>、CTE 把进程打死。

// bindPortal 发送 Bind（可带参数与结果格式）+ Describe(portal) 的 Bind 部分。
func bindPortal(c *testClient, stmt string, params []string, resultFormat int16) {
	p := tstr("")
	p = append(p, tstr(stmt)...)
	p = binary.BigEndian.AppendUint16(p, 0) // 参数全文本
	p = binary.BigEndian.AppendUint16(p, uint16(len(params)))
	for _, v := range params {
		p = binary.BigEndian.AppendUint32(p, uint32(len(v)))
		p = append(p, v...)
	}
	p = binary.BigEndian.AppendUint16(p, 1) // 一个结果格式，作用到所有列
	p = binary.BigEndian.AppendUint16(p, uint16(resultFormat))
	c.sendRaw(pgMsg('B', p))
}

// describePortal 发送 Bind + Describe(portal)，返回 RowDescription 的字段信息。
func describePortal(t *testing.T, c *testClient, stmt string, params []string, resultFormat int16) []field {
	t.Helper()
	bindPortal(c, stmt, params, resultFormat)
	c.sendRaw(testDescribe('P', ""))
	c.sendRaw(testSync())
	return fieldsOf(t, c.readUntil('Z'))
}

// fieldsOf 解析 RowDescription（只取名字、类型 OID 与格式码）。
func fieldsOf(t *testing.T, msgs []msg) []field {
	t.Helper()
	for _, m := range msgs {
		if m.typ != 'T' {
			continue
		}
		n := int(binary.BigEndian.Uint16(m.body))
		off := 2
		out := make([]field, 0, n)
		for i := 0; i < n; i++ {
			name, next := nextCStr(m.body, off)
			off = next
			f := field{Name: name}
			f.TableOID = int32(binary.BigEndian.Uint32(m.body[off:]))
			off += 4
			f.AttNum = int16(binary.BigEndian.Uint16(m.body[off:]))
			off += 2
			f.TypeOID = int32(binary.BigEndian.Uint32(m.body[off:]))
			off += 4
			f.TypeLen = int16(binary.BigEndian.Uint16(m.body[off:]))
			off += 2
			off += 4 // type modifier
			f.Format = int16(binary.BigEndian.Uint16(m.body[off:]))
			off += 2
			out = append(out, f)
		}
		return out
	}
	t.Fatalf("没有 RowDescription: %v", msgs)
	return nil
}

// Describe(portal) 必须报告 Bind 里要求的格式，否则客户端会把二进制当文本读。
func TestDescribePortalReportsBinaryFormat(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	c.sendRaw(testParse("b1", "SELECT id, name FROM t ORDER BY id", nil))
	fields := describePortal(t, c, "b1", nil, 1)
	if len(fields) != 2 {
		t.Fatalf("字段数 = %d, want 2", len(fields))
	}
	for _, f := range fields {
		if f.Format != 1 {
			t.Fatalf("%s 的格式码 = %d, want 1（二进制）", f.Name, f.Format)
		}
	}

	// 要求文本时必须是 0
	fields = describePortal(t, c, "b1", nil, 0)
	for _, f := range fields {
		if f.Format != 0 {
			t.Fatalf("%s 的格式码 = %d, want 0（文本）", f.Name, f.Format)
		}
	}
}

// 二进制结果的值编码：int4 / numeric / text / bool / timestamp。
func TestBinaryResultValues(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	sqlText := "SELECT 1::int4 AS i, 2.5::numeric AS d, 'x'::text AS s, true AS b, '2026-10-27 12:34:56'::timestamp AS ts"
	c.sendRaw(testParse("b2", sqlText, nil))
	describePortal(t, c, "b2", nil, 1)
	c.sendRaw(testExecute(""))
	c.sendRaw(testSync())
	rows := dataRows(c.readUntil('Z'))
	if len(rows) != 1 {
		t.Fatalf("行数 = %d, want 1", len(rows))
	}
	row := rows[0]
	if string(row[0]) != string([]byte{0, 0, 0, 1}) {
		t.Fatalf("int4 = % x, want 00 00 00 01", row[0])
	}
	// numeric 用解码函数回读，避免手写基数块
	if got, err := numericBinaryText(row[1]); err != nil || got != "2.5" {
		t.Fatalf("numeric = % x (%v, %q), want 2.5", row[1], err, got)
	}
	if string(row[2]) != "x" {
		t.Fatalf("text = %q", row[2])
	}
	if len(row[3]) != 1 || row[3][0] != 1 {
		t.Fatalf("bool = % x, want 01", row[3])
	}
	want := mustParsePGTime(t, "2026-10-27 12:34:56")
	if got := int64(binary.BigEndian.Uint64(row[4])); got != microsFromEpoch(want) {
		t.Fatalf("timestamp = %d, want %d", got, microsFromEpoch(want))
	}
}

func mustParsePGTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, ok := timeOf(s)
	if !ok {
		t.Fatalf("解析时间失败: %q", s)
	}
	return v
}

// 带表扫描的 CTE 在 Describe 阶段不能再执行（以前会 nil 解引用打死进程）。
func TestDescribeCTEDoesNotExecute(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	c.sendRaw(testParse("c1", "WITH x AS (SELECT id FROM t) SELECT count(*) AS n FROM x", nil))
	c.sendRaw(testDescribe('S', "c1"))
	c.sendRaw(testSync())
	fields := fieldsOf(t, c.readUntil('Z'))
	if len(fields) != 1 || fields[0].Name != "n" {
		t.Fatalf("Describe 结果 = %v", fields)
	}

	// 连接和进程都还活着，而且真执行时结果正确
	c.sendRaw(testBind("", "c1", nil))
	c.sendRaw(testExecute(""))
	c.sendRaw(testSync())
	rows := dataRows(c.readUntil('Z'))
	if len(rows) != 1 || string(rows[0][0]) != "3" {
		t.Fatalf("CTE 行 = %v", rows)
	}
}

// DML 的 RETURNING 在 Describe 阶段必须给出列定义（否则客户端协议状态错乱）。
func TestDescribeDMLReturning(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	c.sendRaw(testParse("r1", "INSERT INTO t (name, score) VALUES ('dave', 60) RETURNING id, name", nil))
	c.sendRaw(testDescribe('S', "r1"))
	c.sendRaw(testSync())
	fields := fieldsOf(t, c.readUntil('Z'))
	if len(fields) != 2 || fields[0].Name != "id" || fields[1].Name != "name" {
		t.Fatalf("RETURNING 列 = %v", fields)
	}
	if fields[0].TypeOID != 23 {
		t.Fatalf("id 的 OID = %d, want 23", fields[0].TypeOID)
	}

	c.sendRaw(testBind("", "r1", nil))
	c.sendRaw(testExecute(""))
	c.sendRaw(testSync())
	rows := dataRows(c.readUntil('Z'))
	if len(rows) != 1 || string(rows[0][1]) != "dave" {
		t.Fatalf("RETURNING 行 = %v", rows)
	}

	// 不带 RETURNING 的 DML 仍然回 NoData
	c.sendRaw(testParse("r2", "UPDATE t SET score = 10 WHERE name = 'nobody'", nil))
	c.sendRaw(testDescribe('S', "r2"))
	c.sendRaw(testSync())
	for _, m := range c.readUntil('Z') {
		if m.typ == 'T' {
			t.Fatalf("没有 RETURNING 却回了 RowDescription")
		}
	}
}

// numeric 二进制编码：编码后再用现成的解码器读回来，必须是同一个十进制数。
func TestBinaryNumericRoundTrip(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{2.5, "2.5"},
		{int64(0), "0"},
		{-0.001, "-0.001"},
		{int64(10000), "10000"},
		{0.1, "0.1"},
		{1234567890123.45, "1234567890123.45"},
		{-12345678.9, "-12345678.9"},
		{int64(-1), "-1"},
		{0.00001, "0.00001"},
	}
	for _, c := range cases {
		enc, err := encodeBinaryValue(c.in, 1700)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		got, err := numericBinaryText(enc)
		if err != nil {
			t.Fatalf("%v: 解码失败 %v (% x)", c.in, err, enc)
		}
		if strings.Trim(got, "'") != c.want {
			t.Fatalf("%v: 编解码后 = %q, want %q (% x)", c.in, got, c.want, enc)
		}
	}

	// NaN / Infinity 只有 8 字节头
	if got, _ := encodeBinaryValue(math.NaN(), 1700); len(got) != 8 || binary.BigEndian.Uint16(got[4:]) != 0xC000 {
		t.Fatalf("NaN 编码 = % x", got)
	}
	if got, _ := encodeBinaryValue(math.Inf(1), 1700); len(got) != 8 || binary.BigEndian.Uint16(got[4:]) != 0xD000 {
		t.Fatalf("+Inf 编码 = % x", got)
	}
}

// 数组二进制编码：头 + 维度 + 元素。用 Npgsql 相同的读法校验，
// 避免出现「结构能发出去但客户端解不开」。
func TestBinaryArray(t *testing.T) {
	readArray := func(b []byte) (elemOID int32, ndim int, elems []string, nulls []bool) {
		t.Helper()
		if len(b) < 12 {
			t.Fatalf("数组头长度不足: %d", len(b))
		}
		ndim = int(int32(binary.BigEndian.Uint32(b[0:4])))
		elemOID = int32(binary.BigEndian.Uint32(b[8:12]))
		pos := 12
		if ndim == 0 {
			return elemOID, 0, nil, nil
		}
		n := int(int32(binary.BigEndian.Uint32(b[pos : pos+4])))
		pos += 8 // 维度长度 + 下界
		for i := 0; i < n; i++ {
			ln := int(int32(binary.BigEndian.Uint32(b[pos : pos+4])))
			pos += 4
			if ln < 0 {
				elems = append(elems, "")
				nulls = append(nulls, true)
				continue
			}
			elems = append(elems, string(b[pos:pos+ln]))
			nulls = append(nulls, false)
			pos += ln
		}
		return elemOID, ndim, elems, nulls
	}

	enc, err := encodeBinaryValue("{1,2,3}", 1007) // int4[]
	if err != nil {
		t.Fatalf("int4[]: %v", err)
	}
	elemOID, ndim, elems, _ := readArray(enc)
	if elemOID != 23 || ndim != 1 || len(elems) != 3 {
		t.Fatalf("int4[] 头不对: elemOID=%d ndim=%d elems=%v", elemOID, ndim, elems)
	}
	for i, want := range []int32{1, 2, 3} {
		if len(elems[i]) != 4 {
			t.Fatalf("int4[] 第 %d 个元素长度 = %d，期望 4", i, len(elems[i]))
		}
		if got := int32(binary.BigEndian.Uint32([]byte(elems[i]))); got != want {
			t.Fatalf("int4[] 第 %d 个元素 = %d, want %d", i, got, want)
		}
	}

	// 带 NULL 的元素：长度写成 -1
	enc, err = encodeBinaryValue("{1,NULL,3}", 1007)
	if err != nil {
		t.Fatalf("int4[] 带 NULL: %v", err)
	}
	if got := binary.BigEndian.Uint32(enc[4:8]); got != 1 {
		t.Fatalf("hasnull 标志 = %d，期望 1", got)
	}
	_, _, _, nulls := readArray(enc)
	if len(nulls) != 3 || !nulls[1] || nulls[0] || nulls[2] {
		t.Fatalf("NULL 位置不对: %v", nulls)
	}

	// text[] 里的引号与逗号
	enc, err = encodeBinaryValue(`{a,"b,c"}`, 1009)
	if err != nil {
		t.Fatalf("text[]: %v", err)
	}
	_, _, elems, _ = readArray(enc)
	if len(elems) != 2 || elems[0] != "a" || elems[1] != "b,c" {
		t.Fatalf("text[] 元素 = %v，期望 [a b,c]", elems)
	}

	// 空数组：ndim=0
	enc, err = encodeBinaryValue("{}", 1007)
	if err != nil {
		t.Fatalf("空数组: %v", err)
	}
	if elemOID, ndim, _, _ = readArray(enc); ndim != 0 || elemOID != 23 {
		t.Fatalf("空数组头不对: ndim=%d elemOID=%d", ndim, elemOID)
	}

	// 多维暂不支持，要明确报错而不是发出错位的字节
	if _, err := encodeBinaryValue("{{1,2},{3,4}}", 1007); err == nil {
		t.Fatal("多维数组应该报错")
	}
}

// interval 二进制：微秒 + 天 + 月 三段。
func TestBinaryInterval(t *testing.T) {
	cases := []struct {
		in            string
		wantMon, wantD int32
		wantMicros    int64
	}{
		{"1 day", 0, 1, 0},
		{"00:00:00", 0, 0, 0},
		{"1 year 2 mons 3 days 04:05:06", 14, 3, 4*3600e6 + 5*60e6 + 6e6},
		{"-3 hours", 0, 0, -3 * 3600e6},
	}
	for _, c := range cases {
		enc, err := encodeBinaryValue(c.in, 1186)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if len(enc) != 16 {
			t.Fatalf("%q 编码长度 = %d，期望 16", c.in, len(enc))
		}
		micros := int64(binary.BigEndian.Uint64(enc[0:8]))
		days := int32(binary.BigEndian.Uint32(enc[8:12]))
		mons := int32(binary.BigEndian.Uint32(enc[12:16]))
		if micros != c.wantMicros || days != c.wantD || mons != c.wantMon {
			t.Fatalf("%q -> micros=%d days=%d mons=%d，期望 micros=%d days=%d mons=%d",
				c.in, micros, days, mons, c.wantMicros, c.wantD, c.wantMon)
		}
	}
}

// 声明成 int4 却装不下的值必须报错，不能按 4 字节截断 ——
// 截断的结果是一本正经的错数字，客户端和人都看不出来。
func TestBinaryIntOverflow(t *testing.T) {
	if _, err := encodeBinaryValue(int64(9223372036854775807), 23); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Fatalf("int4 越界应报 out of range，实际: %v", err)
	}
	if _, err := encodeBinaryValue(int64(70000), 21); err == nil {
		t.Fatal("int2 越界应报错")
	}
	// int8 正常
	if enc, err := encodeBinaryValue(int64(9223372036854775807), 20); err != nil || len(enc) != 8 {
		t.Fatalf("int8 编码失败: %v %d", err, len(enc))
	}
}
