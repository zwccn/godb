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
