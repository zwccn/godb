package pgwire

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"godb/db"
	"godb/exec"
	"godb/sql"
	"godb/txn"
	"godb/types"
)

const (
	pgServerVersion = "13.0"
	godbVersion     = "0.1.0"

	protoVersion3 = 196608
	codeSSL       = 80877103
	codeGSSENC    = 80877104
)

// Server 是一个 PostgreSQL 协议监听器，SQL 交给 db.DB 执行。
type Server struct {
	DB   *db.DB
	Addr string
	// Logf 用于打印连接级日志，可为 nil。
	Logf func(format string, args ...interface{})
	// QueryLog 非 nil 时，把客户端发来的每条 SQL 追加写进去，
	// 用来排查“某个客户端发了我没实现的语句”这类问题。
	QueryLog io.Writer
}

// logQuery 记录一条来自客户端的 SQL。
func (s *Server) logQuery(text string) {
	if s.QueryLog == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if !strings.HasSuffix(text, ";") {
		text += ";"
	}
	fmt.Fprintf(s.QueryLog, "-- %s\n%s\n\n", time.Now().Format("15:04:05.000"), text)
}

// ListenAndServe 阻塞式监听。
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve 在已有 listener 上服务（测试用）。
func (s *Server) Serve(ln net.Listener) error {
	s.logf("PostgreSQL 协议监听: %s", ln.Addr())
	for {
		nc, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(nc)
	}
}

func (s *Server) logf(format string, args ...interface{}) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

type preparedStmt struct {
	query     string
	paramOIDs []int32
}

type portal struct {
	stmt *preparedStmt
	sql  string
}

type conn struct {
	srv *Server
	nc  net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	user string
	sess *txn.Session

	prepared map[string]*preparedStmt
	portals  map[string]*portal
	skipSync bool // 出错后丢弃消息直到 Sync
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	c := &conn{
		srv:      s,
		nc:       nc,
		br:       bufio.NewReaderSize(nc, 16*1024),
		bw:       bufio.NewWriterSize(nc, 16*1024),
		prepared: map[string]*preparedStmt{},
		portals:  map[string]*portal{},
	}
	if err := c.startup(); err != nil {
		return
	}
	c.sess = s.DB.Session("pg-" + randHex())
	defer func() {
		if c.sess != nil && c.sess.InTx() {
			c.sess.ForceRollback()
		}
	}()
	c.loop()
}

func randHex() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b)
}

// startup 处理 SSLRequest 循环 + StartupMessage + 认证。
func (c *conn) startup() error {
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(lenBuf[:]))
		if n < 8 || n > 1<<20 {
			return fmt.Errorf("bad startup length %d", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(c.br, body); err != nil {
			return err
		}
		code := binary.BigEndian.Uint32(body[:4])
		switch code {
		case codeSSL, codeGSSENC:
			// 不提供加密，让客户端回落到明文
			if _, err := c.nc.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		}
		if code>>16 != 3 {
			c.write(encodeError("FATAL", "0A000",
				fmt.Sprintf("unsupported frontend protocol %d.%d", code>>16, code&0xffff)))
			return fmt.Errorf("unsupported protocol")
		}
		// 解析 key/value 参数，跳过结尾的 \0\0
		rest := body[4:]
		for len(rest) > 0 && rest[0] != 0 {
			kEnd := indexByte(rest, 0)
			if kEnd < 0 {
				break
			}
			key := string(rest[:kEnd])
			rest = rest[kEnd+1:]
			vEnd := indexByte(rest, 0)
			if vEnd < 0 {
				break
			}
			val := string(rest[:vEnd])
			rest = rest[vEnd+1:]
			if key == "user" {
				c.user = val
			}
		}
		break
	}
	if c.user == "" {
		c.user = "godb"
	}

	// trust 认证：直接放行
	var out [][]byte
	out = append(out, encodeAuthenticationOK())
	params := [][2]string{
		{"server_version", pgServerVersion},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"application_name", "godb"},
		{"is_superuser", "off"},
		{"session_authorization", c.user},
		{"DateStyle", "ISO, MDY"},
		{"IntervalStyle", "postgres"},
		{"TimeZone", "UTC"},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
	}
	for _, p := range params {
		out = append(out, encodeParameterStatus(p[0], p[1]))
	}
	out = append(out, encodeBackendKeyData(1, 0))
	out = append(out, encodeReadyForQuery('I'))
	return c.write(out...)
}

func indexByte(b []byte, v byte) int {
	for i := range b {
		if b[i] == v {
			return i
		}
	}
	return -1
}

func (c *conn) write(msgs ...[]byte) error {
	for _, m := range msgs {
		if _, err := c.bw.Write(m); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

func (c *conn) readMsg() (byte, []byte, error) {
	typ, err := c.br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n < 4 || n > 1<<24 {
		return 0, nil, fmt.Errorf("bad message length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return typ, payload, nil
}

func (c *conn) txStatus() byte {
	if c.sess == nil {
		return 'I'
	}
	if !c.sess.InTx() {
		return 'I'
	}
	if c.sess.Aborted() {
		return 'E'
	}
	return 'T'
}

func (c *conn) loop() {
	for {
		typ, payload, err := c.readMsg()
		if err != nil {
			return
		}
		if c.skipSync && typ != 'S' && typ != 'X' {
			continue
		}
		switch typ {
		case 'Q':
			c.handleSimpleQuery(payload)
		case 'P':
			c.handleParse(payload)
		case 'B':
			c.handleBind(payload)
		case 'D':
			c.handleDescribe(payload)
		case 'E':
			c.handleExecute(payload)
		case 'S':
			c.skipSync = false
			_ = c.write(encodeReadyForQuery(c.txStatus()))
		case 'H': // Flush
			_ = c.bw.Flush()
		case 'C':
			c.handleClose(payload)
		case 'X':
			return
		default:
			// 忽略不认识的类型（如 COPY 相关）
		}
	}
}

// ---------- 简单查询 ----------

func (c *conn) handleSimpleQuery(payload []byte) {
	text := cstr(payload, 0)
	if strings.TrimSpace(text) == "" {
		_ = c.write(encodeEmptyQuery(), encodeReadyForQuery(c.txStatus()))
		return
	}
	c.srv.logQuery(text)
	if msgs, ok := c.compat(text); ok {
		msgs = append(msgs, encodeReadyForQuery(c.txStatus()))
		_ = c.write(msgs...)
		return
	}
	results, err := c.srv.DB.Exec(c.sess, text)
	var out [][]byte
	for _, r := range results {
		out = append(out, encodeResult(r)...)
	}
	if err != nil {
		out = append(out, c.errorResponse(err)...)
	}
	out = append(out, encodeReadyForQuery(c.txStatus()))
	_ = c.write(out...)
}

func encodeResult(r *exec.Result) [][]byte {
	var out [][]byte
	if len(r.Columns) > 0 {
		fields := make([]field, len(r.Columns))
		for i, col := range r.Columns {
			oid, size := typeOID(col.Type)
			fields[i] = field{Name: col.Name, TypeOID: oid, TypeLen: size}
		}
		out = append(out, encodeRowDescription(fields))
		for _, row := range r.Rows {
			vals := make([][]byte, len(row))
			for i, v := range row {
				vals[i] = textValue(v)
			}
			out = append(out, encodeDataRow(vals))
		}
	}
	tag := r.Tag
	if tag == "" {
		tag = r.Command
	}
	if tag != "" {
		out = append(out, encodeCommandComplete(tag))
	}
	return out
}

func (c *conn) errorResponse(err error) [][]byte {
	code, msg := mapError(err)
	return [][]byte{encodeError("ERROR", code, msg)}
}

// ---------- 扩展查询 ----------

func cstr(b []byte, off int) string {
	if off >= len(b) {
		return ""
	}
	for i := off; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[off:i])
		}
	}
	return string(b[off:])
}

// nextCStr 返回字符串与新的偏移。
func nextCStr(b []byte, off int) (string, int) {
	for i := off; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[off:i]), i + 1
		}
	}
	return string(b[off:]), len(b)
}

func (c *conn) handleParse(payload []byte) {
	name, off := nextCStr(payload, 0)
	query, off := nextCStr(payload, off)
	var oids []int32
	if off+2 <= len(payload) {
		n := int(int16(binary.BigEndian.Uint16(payload[off:])))
		off += 2
		for i := 0; i < n && off+4 <= len(payload); i++ {
			oids = append(oids, int32(binary.BigEndian.Uint32(payload[off:])))
			off += 4
		}
	}
	c.prepared[name] = &preparedStmt{query: query, paramOIDs: oids}
	_ = c.write(encodeParseComplete())
}

func (c *conn) handleBind(payload []byte) {
	portalName, off := nextCStr(payload, 0)
	stmtName, off := nextCStr(payload, off)
	stmt := c.prepared[stmtName]
	if stmt == nil {
		c.fail(fmt.Errorf("prepared statement %q does not exist", stmtName))
		return
	}
	var paramFormats []int16
	if off+2 <= len(payload) {
		n := int(int16(binary.BigEndian.Uint16(payload[off:])))
		off += 2
		for i := 0; i < n && off+2 <= len(payload); i++ {
			paramFormats = append(paramFormats, int16(binary.BigEndian.Uint16(payload[off:])))
			off += 2
		}
	}
	var params [][]byte
	if off+2 <= len(payload) {
		n := int(int16(binary.BigEndian.Uint16(payload[off:])))
		off += 2
		for i := 0; i < n && off+4 <= len(payload); i++ {
			l := int32(binary.BigEndian.Uint32(payload[off:]))
			off += 4
			if l < 0 {
				params = append(params, nil)
				continue
			}
			if off+int(l) > len(payload) {
				break
			}
			v := make([]byte, l)
			copy(v, payload[off:off+int(l)])
			off += int(l)
			params = append(params, v)
		}
	}
	sqlText, err := substituteParams(stmt.query, params, stmt.paramOIDs, paramFormats)
	if err != nil {
		c.fail(err)
		return
	}
	c.portals[portalName] = &portal{stmt: stmt, sql: sqlText}
	_ = c.write(encodeBindComplete())
}

func (c *conn) handleDescribe(payload []byte) {
	if len(payload) < 2 {
		c.fail(fmt.Errorf("bad Describe message"))
		return
	}
	kind := payload[0]
	name := cstr(payload, 1)
	var sqlText string
	var stmt *preparedStmt
	switch kind {
	case 'S':
		stmt = c.prepared[name]
		if stmt == nil {
			c.fail(fmt.Errorf("prepared statement %q does not exist", name))
			return
		}
		sqlText = stmt.query
	case 'P':
		p := c.portals[name]
		if p == nil {
			c.fail(fmt.Errorf("portal %q does not exist", name))
			return
		}
		stmt = p.stmt
		sqlText = p.sql
	default:
		c.fail(fmt.Errorf("bad Describe kind %q", kind))
		return
	}

	var out [][]byte
	if kind == 'S' {
		out = append(out, encodeParameterDescription(stmt.paramOIDs))
	}
	fields, hasRows := c.fieldsFor(sqlText)
	if !hasRows {
		out = append(out, encodeNoData())
	} else {
		out = append(out, encodeRowDescription(fields))
	}
	_ = c.write(out...)
}

func (c *conn) handleExecute(payload []byte) {
	name, off := nextCStr(payload, 0)
	_ = off // maxRows 忽略：我们一次性返回全部行
	p := c.portals[name]
	if p == nil {
		c.fail(fmt.Errorf("portal %q does not exist", name))
		return
	}
	if strings.TrimSpace(p.sql) == "" {
		_ = c.write(encodeEmptyQuery())
		return
	}
	if msgs, ok := c.compat(p.sql); ok {
		_ = c.write(msgs...)
		return
	}
	results, err := c.srv.DB.Exec(c.sess, p.sql)
	var out [][]byte
	for _, r := range results {
		out = append(out, encodeResult(r)...)
	}
	if err != nil {
		out = append(out, c.errorResponse(err)...)
		c.skipSync = true
	}
	_ = c.write(out...)
}

func (c *conn) handleClose(payload []byte) {
	if len(payload) < 2 {
		return
	}
	kind := payload[0]
	name := cstr(payload, 1)
	if kind == 'S' {
		delete(c.prepared, name)
	} else {
		delete(c.portals, name)
	}
	_ = c.write(encodeCloseComplete())
}

// fail 发送错误并进入“等 Sync”状态。
func (c *conn) fail(err error) {
	_ = c.write(c.errorResponse(err)...)
	c.skipSync = true
}

// fieldsFor 在不执行的前提下推断输出列（供 Describe 使用）。
func (c *conn) fieldsFor(sqlText string) ([]field, bool) {
	norm := normalize(sqlText)
	if norm == "" {
		return nil, false
	}
	// 与 compat 里的探针保持一致
	if strings.HasPrefix(norm, "select version()") || strings.HasPrefix(norm, "select pg_catalog.version()") {
		return []field{{Name: "version", TypeOID: 25, TypeLen: -1}}, true
	}
	if strings.HasPrefix(norm, "select current_schema()") {
		return []field{{Name: "current_schema", TypeOID: 25, TypeLen: -1}}, true
	}
	if strings.HasPrefix(norm, "select current_database()") {
		return []field{{Name: "current_database", TypeOID: 25, TypeLen: -1}}, true
	}
	if strings.HasPrefix(norm, "select pg_catalog.set_config") {
		return []field{{Name: "set_config", TypeOID: 25, TypeLen: -1}}, true
	}
	if strings.HasPrefix(norm, "show ") {
		return []field{{Name: strings.TrimSpace(strings.TrimPrefix(norm, "show ")), TypeOID: 25, TypeLen: -1}}, true
	}

	stmts, err := sql.Parse(replaceParamsWithNull(sqlText))
	if err != nil || len(stmts) == 0 {
		return nil, false
	}
	sc, err := exec.Describe(c.srv.DB.Cat, stmts[len(stmts)-1])
	if err != nil || len(sc) == 0 {
		return nil, false
	}
	fields := make([]field, len(sc))
	for i, col := range sc {
		oid, size := typeOID(col.Type.String())
		fields[i] = field{Name: col.Name, TypeOID: oid, TypeLen: size}
	}
	return fields, true
}

// ---------- 类型映射与文本编码 ----------

// typeOID 把我们的类型名映射成 PG 的 OID。
// typeOID 返回 PG 类型 OID 与 typlen，统一走 types 包，避免两处映射不一致。
func typeOID(t string) (int32, int16) {
	return types.OIDByName(t), int16(types.TypeLenByName(t))
}

// textValue 把结果值编码成 PG 文本格式。
func textValue(v interface{}) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case int64:
		return []byte(strconv.FormatInt(x, 10))
	case int:
		return []byte(strconv.Itoa(x))
	case float64:
		return []byte(strconv.FormatFloat(x, 'g', -1, 64))
	case bool:
		if x {
			return []byte("t")
		}
		return []byte("f")
	case string:
		return []byte(x)
	case []byte:
		return x
	}
	return []byte(fmt.Sprint(v))
}

// ---------- 错误码映射 ----------

func mapError(err error) (string, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "syntax error") || strings.Contains(msg, "unexpected"):
		return "42601", msg
	case strings.Contains(msg, "relation") && strings.Contains(msg, "does not exist"):
		return "42P01", msg
	case strings.Contains(msg, "column") && strings.Contains(msg, "does not exist"):
		return "42703", msg
	case strings.Contains(msg, "duplicate key value"):
		return "23505", msg
	case strings.Contains(msg, "not-null constraint"):
		return "23502", msg
	case strings.Contains(msg, "死锁"):
		return "40P01", msg
	case strings.Contains(msg, "等待锁超时"):
		return "55P03", msg
	case strings.Contains(msg, "aborted"):
		return "25P02", msg
	case strings.Contains(msg, "cannot run DDL inside"):
		return "25001", msg
	}
	return "42000", msg
}
