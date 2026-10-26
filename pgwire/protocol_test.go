package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"godb/db"
	"godb/lock"
)

// ---------- 测试用的极简 PG 客户端 ----------

type msg struct {
	typ  byte
	body []byte
}

type testClient struct {
	conn net.Conn
	t    *testing.T
}

func startServer(t *testing.T) (*db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(dir, db.Options{Lock: lock.Options{
		Timeout: 500 * time.Millisecond, Retry: 10 * time.Millisecond,
		DeadlockDetect: true, DeadlockAfter: 100 * time.Millisecond,
	}})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if _, err := d.Exec(d.Session("init"),
		`CREATE TABLE t (id SERIAL PRIMARY KEY, name TEXT, score INTEGER);
		 INSERT INTO t (name, score) VALUES ('alice', 90), ('bob', 80), ('carol', 70);`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{DB: d, Logf: func(string, ...interface{}) {}}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return d, ln.Addr().String()
}

func dial(t *testing.T, addr string) *testClient {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &testClient{conn: conn, t: t}
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// StartupMessage: len + 196608 + user\0zwccn\0\0
	body := make([]byte, 0, 64)
	body = binary.BigEndian.AppendUint32(body, protoVersion3)
	body = append(body, []byte("user\x00zwccn\x00")...)
	body = append(body, 0)
	startup := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	startup = append(startup, body...)
	if _, err := conn.Write(startup); err != nil {
		t.Fatalf("write startup: %v", err)
	}

	msgs := c.readUntil('Z')
	var sawAuthOK bool
	for _, m := range msgs {
		if m.typ == 'R' && len(m.body) >= 4 && binary.BigEndian.Uint32(m.body) == 0 {
			sawAuthOK = true
		}
		if m.typ == 'E' {
			t.Fatalf("startup failed: %s", string(m.body))
		}
	}
	if !sawAuthOK {
		t.Fatalf("no AuthenticationOk in startup response")
	}
	return c
}

func (c *testClient) close() { c.conn.Close() }

func (c *testClient) readMsg() msg {
	c.t.Helper()
	var hdr [5]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		c.t.Fatalf("read header: %v", err)
	}
	n := int(binary.BigEndian.Uint32(hdr[1:]))
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return msg{typ: hdr[0], body: body}
}

// readUntil 一直读到指定类型为止（含）。
func (c *testClient) readUntil(until byte) []msg {
	c.t.Helper()
	var out []msg
	for i := 0; i < 200; i++ {
		m := c.readMsg()
		out = append(out, m)
		if m.typ == until {
			return out
		}
	}
	c.t.Fatalf("did not see message %c", until)
	return nil
}

func (c *testClient) sendRaw(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// query 走简单查询协议。
func (c *testClient) query(sql string) []msg {
	c.t.Helper()
	p := append([]byte(sql), 0)
	c.sendRaw(pgMsg('Q', p))
	return c.readUntil('Z')
}

// ---------- 客户端消息构造 ----------

func pgMsg(typ byte, payload []byte) []byte {
	out := []byte{typ}
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)+4))
	return append(out, payload...)
}

func tstr(s string) []byte { return append([]byte(s), 0) }

func testParse(name, sql string, oids []int32) []byte {
	p := tstr(name)
	p = append(p, tstr(sql)...)
	p = binary.BigEndian.AppendUint16(p, uint16(len(oids)))
	for _, o := range oids {
		p = binary.BigEndian.AppendUint32(p, uint32(o))
	}
	return pgMsg('P', p)
}

func testBind(portal, stmt string, params []string) []byte {
	p := tstr(portal)
	p = append(p, tstr(stmt)...)
	p = binary.BigEndian.AppendUint16(p, 0) // 全部文本格式
	p = binary.BigEndian.AppendUint16(p, uint16(len(params)))
	for _, v := range params {
		if v == "\x00NULL" {
			p = binary.BigEndian.AppendUint32(p, 0xffffffff)
			continue
		}
		p = binary.BigEndian.AppendUint32(p, uint32(len(v)))
		p = append(p, v...)
	}
	p = binary.BigEndian.AppendUint16(p, 0) // 结果也用文本
	return pgMsg('B', p)
}

func testDescribe(kind byte, name string) []byte {
	p := []byte{kind}
	p = append(p, tstr(name)...)
	return pgMsg('D', p)
}

func testExecute(portal string) []byte {
	p := tstr(portal)
	p = binary.BigEndian.AppendUint32(p, 0)
	return pgMsg('E', p)
}

func testSync() []byte { return pgMsg('S', nil) }

// ---------- 断言辅助 ----------

func dataRows(msgs []msg) [][][]byte {
	var out [][][]byte
	for _, m := range msgs {
		if m.typ != 'D' {
			continue
		}
		n := int(binary.BigEndian.Uint16(m.body))
		off := 2
		row := make([][]byte, 0, n)
		for i := 0; i < n; i++ {
			l := int32(binary.BigEndian.Uint32(m.body[off:]))
			off += 4
			if l < 0 {
				row = append(row, nil)
				continue
			}
			row = append(row, m.body[off:off+int(l)])
			off += int(l)
		}
		out = append(out, row)
	}
	return out
}

func cmdTag(msgs []msg) string {
	for _, m := range msgs {
		if m.typ == 'C' {
			return strings.TrimRight(string(m.body), "\x00")
		}
	}
	return ""
}

func errorMsg(msgs []msg) string {
	for _, m := range msgs {
		if m.typ == 'E' {
			// 解析 'M' 字段
			b := m.body
			for i := 0; i < len(b)-1; i++ {
				if b[i] == 'M' {
					end := strings.IndexByte(string(b[i+1:]), 0)
					if end >= 0 {
						return string(b[i+1 : i+1+end])
					}
				}
			}
			return string(b)
		}
	}
	return ""
}

// ---------- 用例 ----------

func TestSimpleQuery(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	msgs := c.query("SELECT id, name FROM t ORDER BY id")
	if got := cmdTag(msgs); got != "SELECT 3" {
		t.Fatalf("CommandComplete = %q, want SELECT 3", got)
	}
	rows := dataRows(msgs)
	if len(rows) != 3 || string(rows[0][1]) != "alice" {
		t.Fatalf("rows = %v", rows)
	}
	// RowDescription 必须有
	found := false
	for _, m := range msgs {
		if m.typ == 'T' {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing RowDescription")
	}
}

func TestSimpleQueryMultiStatement(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	msgs := c.query("BEGIN; UPDATE t SET score = 99 WHERE id = 1; COMMIT;")
	var tags []string
	for _, m := range msgs {
		if m.typ == 'C' {
			tags = append(tags, strings.TrimRight(string(m.body), "\x00"))
		}
	}
	want := []string{"BEGIN", "UPDATE 1", "COMMIT"}
	if fmt.Sprint(tags) != fmt.Sprint(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}

func TestTransactionStatus(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	c.query("BEGIN")
	msgs := c.query("SELECT 1")
	last := msgs[len(msgs)-1]
	if last.typ != 'Z' || last.body[0] != 'T' {
		t.Fatalf("ReadyForQuery status = %q, want T", last.body)
	}
	// 事务内出错 -> 'E'
	msgs = c.query("SELECT * FROM nope")
	if s := msgs[len(msgs)-1]; s.typ != 'Z' || s.body[0] != 'E' {
		t.Fatalf("after error status = %q, want E", s.body)
	}
	msgs = c.query("ROLLBACK")
	if s := msgs[len(msgs)-1]; s.body[0] != 'I' {
		t.Fatalf("after rollback status = %q, want I", s.body)
	}
}

func TestErrorResponse(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	msgs := c.query("SELECT * FROM no_such_table")
	if e := errorMsg(msgs); !strings.Contains(e, "does not exist") {
		t.Fatalf("error = %q", e)
	}
	// 错误后仍能继续查询
	msgs = c.query("SELECT count(*) FROM t")
	if got := cmdTag(msgs); got != "SELECT 1" {
		t.Fatalf("after error: %q", got)
	}
}

func TestExtendedQuery(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	// Parse + Describe(statement) -> 先拿到列定义
	c.sendRaw(testParse("s1", "SELECT name, score FROM t WHERE score > $1 ORDER BY score DESC", []int32{23}))
	c.sendRaw(testDescribe('S', "s1"))
	c.sendRaw(testSync())
	msgs := c.readUntil('Z')

	var sawParseComplete, sawParamDesc, sawRowDesc bool
	for _, m := range msgs {
		switch m.typ {
		case '1':
			sawParseComplete = true
		case 't':
			sawParamDesc = true
			if n := int(binary.BigEndian.Uint16(m.body)); n != 1 {
				t.Fatalf("param count = %d, want 1", n)
			}
		case 'T':
			sawRowDesc = true
			if n := int(binary.BigEndian.Uint16(m.body)); n != 2 {
				t.Fatalf("field count = %d, want 2", n)
			}
		}
	}
	if !sawParseComplete || !sawParamDesc || !sawRowDesc {
		t.Fatalf("missing messages: parse=%v param=%v row=%v", sawParseComplete, sawParamDesc, sawRowDesc)
	}

	// Bind + Execute
	c.sendRaw(testBind("", "s1", []string{"75"}))
	c.sendRaw(testExecute(""))
	c.sendRaw(testSync())
	msgs = c.readUntil('Z')
	// score > 75 -> alice(90), bob(80)
	if got := cmdTag(msgs); got != "SELECT 2" {
		t.Fatalf("tag = %q, want SELECT 2", got)
	}
	rows := dataRows(msgs)
	if len(rows) != 2 || string(rows[0][0]) != "alice" {
		t.Fatalf("rows = %v", rows)
	}

	// 参数换成 85 -> 只剩 1 行（端口名可复用，重新 Bind 一个门户）
	c.sendRaw(testBind("p2", "s1", []string{"85"}))
	c.sendRaw(testExecute("p2"))
	c.sendRaw(testSync())
	msgs = c.readUntil('Z')
	if got := cmdTag(msgs); got != "SELECT 1" {
		t.Fatalf("tag = %q, want SELECT 1", got)
	}
}

func TestExtendedQueryTextParam(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	c.sendRaw(testParse("s2", "SELECT id FROM t WHERE name = $1", []int32{25}))
	c.sendRaw(testBind("", "s2", []string{"bob"}))
	c.sendRaw(testExecute(""))
	c.sendRaw(testSync())
	msgs := c.readUntil('Z')
	rows := dataRows(msgs)
	if len(rows) != 1 || string(rows[0][0]) != "2" {
		t.Fatalf("rows = %v (期望 id=2)", rows)
	}
}

func TestCompatProbes(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT version()", "13.0"},
		{"SHOW server_version", "13.0"},
		{"SELECT current_schema()", "public"},
		{"SELECT pg_catalog.set_config('search_path', '', false)", ""},
	}
	for _, tc := range cases {
		msgs := c.query(tc.sql)
		rows := dataRows(msgs)
		if len(rows) != 1 {
			t.Fatalf("%s -> %d rows", tc.sql, len(rows))
		}
		if !strings.Contains(string(rows[0][0]), tc.want) {
			t.Fatalf("%s -> %q, want contains %q", tc.sql, rows[0][0], tc.want)
		}
	}

	// SET 只回 CommandComplete
	msgs := c.query("SET client_encoding = 'UTF8'")
	if got := cmdTag(msgs); got != "SET" {
		t.Fatalf("SET tag = %q", got)
	}
}

func TestEmptyQuery(t *testing.T) {
	_, addr := startServer(t)
	c := dial(t, addr)
	defer c.close()

	msgs := c.query("")
	found := false
	for _, m := range msgs {
		if m.typ == 'I' {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing EmptyQueryResponse")
	}
}
