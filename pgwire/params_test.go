package pgwire

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// numericBytes 按 PG 的 numeric 二进制格式拼字节：
// ndigits | weight | sign | dscale | ndigits × int16
func numericBytes(weight, sign, dscale int, digits ...int) []byte {
	out := make([]byte, 8+len(digits)*2)
	binary.BigEndian.PutUint16(out[0:], uint16(len(digits)))
	binary.BigEndian.PutUint16(out[2:], uint16(int16(weight)))
	binary.BigEndian.PutUint16(out[4:], uint16(sign))
	binary.BigEndian.PutUint16(out[6:], uint16(dscale))
	for i, d := range digits {
		binary.BigEndian.PutUint16(out[8+i*2:], uint16(d))
	}
	return out
}

func i64Bytes(v int64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, uint64(v))
	return out
}

func microsSincePG(t time.Time) int64 { return t.Sub(pgEpoch).Microseconds() }

// Npgsql 默认用二进制格式绑定参数。以前 numeric / timestamp 之类没解码，
// 二进制字节被当文本拼进 SQL —— 报错信息全是乱码，jsonb/uuid 还可能静默写脏数据。
func TestBinaryNumeric(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"12.5", numericBytes(0, 0, 1, 12, 5000), "12.5"},
		{"整数 0", numericBytes(0, 0, 0), "0"},
		{"负数 -0.001", numericBytes(-1, 0x4000, 3, 10), "-0.001"},
		{"10000", numericBytes(1, 0, 0, 1, 0), "10000"},
		{"1.500 保留尾部 0", numericBytes(0, 0, 3, 1, 5000), "1.500"},
		{"NaN", numericBytes(0, 0xC000, 0), "'NaN'"},
		{"12345678.9", numericBytes(1, 0, 1, 1234, 5678, 9000), "12345678.9"},
		{"大数 1234567890123456", numericBytes(3, 0, 0, 1234, 5678, 9012, 3456), "1234567890123456"},
	}
	for _, c := range cases {
		got, err := binaryLiteral(c.raw, 1700)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBinaryTimestampAndDate(t *testing.T) {
	// 报告里给的锚点：2024-01-01 = 757382400000000 微秒
	raw := i64Bytes(757382400000000)
	got, err := binaryLiteral(raw, 1114)
	if err != nil {
		t.Fatal(err)
	}
	if got != "'2024-01-01 00:00:00'" {
		t.Fatalf("timestamp = %s", got)
	}

	// 2000 年之前（负微秒）：1999-12-31 12:00:00
	if got, _ := binaryLiteral(i64Bytes(microsSincePG(time.Date(1999, 12, 31, 12, 0, 0, 0, time.UTC))), 1114); got != "'1999-12-31 12:00:00'" {
		t.Fatalf("2000 之前 = %s", got)
	}
	// Unix 纪元
	if got, _ := binaryLiteral(i64Bytes(microsSincePG(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))), 1114); got != "'1970-01-01 00:00:00'" {
		t.Fatalf("1970 = %s", got)
	}
	// 带小数秒
	if got, _ := binaryLiteral(i64Bytes(microsSincePG(time.Date(2024, 3, 5, 6, 7, 8, 123456000, time.UTC))), 1114); got != "'2024-03-05 06:07:08.123456'" {
		t.Fatalf("小数秒 = %s", got)
	}
	// timestamptz 走 RFC3339（SQL 层的 ParseTime 认识）
	if got, _ := binaryLiteral(raw, 1184); !strings.HasPrefix(got, "'2024-01-01T00:00:00") {
		t.Fatalf("timestamptz = %s", got)
	}

	// date：2000-01-01 + 8766 天 = 2024-01-01
	dateRaw := make([]byte, 4)
	binary.BigEndian.PutUint32(dateRaw, uint32(8766))
	if got, _ := binaryLiteral(dateRaw, 1082); got != "'2024-01-01'" {
		t.Fatalf("date = %s", got)
	}
	// 2000 之前：-1 天 = 1999-12-31
	var negOne int32 = -1
	binary.BigEndian.PutUint32(dateRaw, uint32(negOne))
	if got, _ := binaryLiteral(dateRaw, 1082); got != "'1999-12-31'" {
		t.Fatalf("date 负数 = %s", got)
	}

	// time：02:03:04.5
	if got, _ := binaryLiteral(i64Bytes(int64(2*3600e6+3*60e6+4e6+500000)), 1083); got != "'02:03:04.5'" {
		t.Fatalf("time = %s", got)
	}
}

func TestBinaryUuidJsonbBytea(t *testing.T) {
	uuid := []byte{
		0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
	}
	if got, err := binaryLiteral(uuid, 2950); err != nil || got != "'12345678-9abc-def0-1122-334455667788'" {
		t.Fatalf("uuid = %s, err = %v", got, err)
	}

	jsonb := append([]byte{1}, []byte(`{"a": 1}`)...)
	if got, err := binaryLiteral(jsonb, 3802); err != nil || got != `'{"a": 1}'` {
		t.Fatalf("jsonb = %s, err = %v", got, err)
	}

	if got, err := binaryLiteral([]byte{0xde, 0xad, 0xbe, 0xef}, 17); err != nil || got != `'\xdeadbeef'` {
		t.Fatalf("bytea = %s, err = %v", got, err)
	}

	// 文本编码就等于字节的类型
	if got, err := binaryLiteral([]byte("hello"), 25); err != nil || got != "'hello'" {
		t.Fatalf("text = %s, err = %v", got, err)
	}
}

// 没实现的类型必须明确报错：静默把二进制当文本会写进垃圾数据。
func TestBinaryUnsupportedTypeFails(t *testing.T) {
	_, err := binaryLiteral([]byte{0, 0, 0, 1}, 600) // point
	if err == nil {
		t.Fatal("未支持的类型应当报错")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("错误信息 = %q", err)
	}
}

// 同一批参数里文本与二进制混合（formats 可以逐参数指定），别只看第一个。
func TestSubstituteMixedFormats(t *testing.T) {
	params := [][]byte{[]byte("42"), numericBytes(0, 0, 1, 12, 5000), []byte("hi")}
	oids := []int32{23, 1700, 25}
	formats := []int16{0, 1, 0}

	got, err := substituteParams("INSERT INTO t VALUES ($1, $2, $3)", params, oids, formats)
	if err != nil {
		t.Fatalf("substitute: %v", err)
	}
	want := "INSERT INTO t VALUES (42, 12.5, 'hi')"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 结果值的文本编码：numeric 必须定点（PG 从不给指数），date 列只出日期。
func TestTextValueFormats(t *testing.T) {
	cases := []struct {
		v       interface{}
		colType string
		want    string
	}{
		{1234567890123.45, "numeric", "1234567890123.45"},
		{1.5, "numeric", "1.5"},
		{-0.001, "numeric", "-0.001"},
		{1e20, "float8", "1e+20"}, // float8 保留指数形式（PG 也是）
		{"2024-01-01 00:00:00", "date", "2024-01-01"},
		{"2024-03-05 06:07:08.123456", "timestamp", "2024-03-05 06:07:08.123456"},
		{int64(42), "int4", "42"},
	}
	for _, c := range cases {
		if got := string(textValueAs(c.v, c.colType)); got != c.want {
			t.Fatalf("%v as %s = %q, want %q", c.v, c.colType, got, c.want)
		}
	}
}
