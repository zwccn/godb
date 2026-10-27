package pgwire

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"godb/db"
	"godb/exec"
	"godb/sql"
	"godb/syscat"
	"godb/txn"
	"godb/types"
)

const (
	// pgServerVersion 是汇报给客户端的版本号。故意报 10.0 而不是 13.0：
	// 目录表本来就是按 PG 11 之前的形状做的（relhasoids、consrc、proisagg 都还在），
	// 老客户端（Navicat 12 是 2017 年的，只认到 PG 10）看到 13.0 会挑不到它认识的
	// 类型表/元数据路径，设计表里所有列类型都退化成 (Type) 对象类型。
	// 报 10.0 和实际暴露的目录形状是一致的，对 modern 客户端也没有影响。
	pgServerVersion = syscat.ServerVersion
	godbVersion     = syscat.GodbVersion

	protoVersion3 = 196608
	codeSSL       = 80877103
	codeGSSENC    = 80877104
)

// Server 是一个 PostgreSQL 协议监听器，SQL 交给 db.DB 执行。
type Server struct {
	// DB 是单库模式下的默认库（测试与老调用方用）。
	// DBs 非空时按连接的 database 参数选库，DB 只作为兜底。
	DB  *db.DB
	DBs *db.Manager

	Addr string
	// Logf 用于打印连接级日志，可为 nil。
	Logf func(format string, args ...interface{})
	// QueryLog 非 nil 时，把客户端发来的每条 SQL 追加写进去，
	// 用来排查“某个客户端发了我没实现的语句”这类问题。
	QueryLog io.Writer

	// AuthRequired 返回当前是否要求密码认证。nil 或返回 false 表示 trust
	// （不校验密码）。用函数而不是布尔值，是为了让设置页改完立即生效，
	// 不需要重建 Server。
	AuthRequired func() bool
	// AuthVerify 校验用户名与密码。返回 false 会回 28P01 并断开连接。
	AuthVerify func(user, password string) bool
	// AuthMD5 返回该用户 md5 挑战应答用的验证值（md5(md5(pw+user))）。
	// 非空则优先用 md5 认证，密码不落线。
	AuthMD5 func(user string) (string, bool)
	// AuthScram 返回 SCRAM-SHA-256 验证串。非空则优先用 SCRAM（最安全）。
	AuthScram func(user string) (string, bool)
	// TLSCert 非 nil 时接受 SSLRequest：回 'S' 并把连接升级成 TLS。
	TLSCert *tls.Certificate
}

// authEnabled 判断当前连接是否需要密码认证。
func (s *Server) authEnabled() bool {
	return s.AuthRequired != nil && s.AuthRequired()
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
			// listener 被主动关闭（热切换端口 / 进程退出）属于正常结束
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
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
	// resultFormats 是 Bind 里客户端要求的结果格式：0=文本、1=二进制。
	// 空表示全文本；1 个值表示所有列同一个格式；否则按列一一对应。
	// 不能忽略：Npgsql（及其上的 EFCore/Dapper）默认要二进制。
	resultFormats []int16
}

type conn struct {
	srv *Server
	nc  net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	user   string
	dbName string // startup 里声明的 database，空表示用默认库
	db     *db.DB // 选中的库，startup 成功后就固定
	sess   *txn.Session

	prepared map[string]*preparedStmt
	portals  map[string]*portal
	skipSync bool // 出错后丢弃消息直到 Sync

	// pid 是给 pg_stat_activity 用的后端编号；unregister 在连接结束时注销
	pid        int32
	unregister func()
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
	c.sess = c.db.Session("pg-" + randHex())
	c.register()
	defer func() {
		if c.sess != nil && c.sess.InTx() {
			c.sess.ForceRollback()
		}
		if c.unregister != nil {
			c.unregister()
		}
	}()
	c.loop()
}

// selectDB 按 startup 里声明的 database 选库。多库模式下库不存在直接 FATAL。
func (c *conn) selectDB() error {
	if c.srv.DBs == nil {
		// 单库模式（测试、嵌入式用法）
		c.db = c.srv.DB
		if c.db == nil {
			return fmt.Errorf("no database available")
		}
		return nil
	}
	d, err := c.srv.DBs.Get(c.dbName)
	if err != nil {
		return err
	}
	c.db = d
	return nil
}

// dbNameOr 返回 pg_stat_activity 里要显示的库名。
func (c *conn) dbNameOr() string {
	if c.db != nil {
		return c.db.Name
	}
	return syscat.DefaultDatabaseName
}

// register 把这条连接登记到 pg_stat_activity。
func (c *conn) register() {
	addr, port := "", int32(0)
	if host, p, err := net.SplitHostPort(c.nc.RemoteAddr().String()); err == nil {
		addr = host
		if n, err := strconv.Atoi(p); err == nil {
			port = int32(n)
		}
	}
	c.pid, c.unregister = syscat.RegisterActivity(syscat.Activity{
		User: c.user, DB: c.dbNameOr(), Addr: addr, Port: port,
		State: "idle", AppName: "godb client",
	})
}

// setActivity 更新 pg_stat_activity 里的状态（查询执行前后各调一次）。
func (c *conn) setActivity(state, query string) {
	if c.pid != 0 {
		syscat.SetActivityState(c.pid, state, query)
	}
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
		case codeSSL:
			if c.srv.TLSCert == nil {
				// 没配证书：回 'N' 让客户端回落到明文
				if _, err := c.nc.Write([]byte{'N'}); err != nil {
					return err
				}
				continue
			}
			// 回 'S' 之后整条连接变成 TLS，后续握手都在加密通道里
			if _, err := c.nc.Write([]byte{'S'}); err != nil {
				return err
			}
			tlsConn := tls.Server(c.nc, &tls.Config{
				Certificates: []tls.Certificate{*c.srv.TLSCert},
				MinVersion:   tls.VersionTLS12,
			})
			if err := tlsConn.Handshake(); err != nil {
				return err
			}
			c.nc = tlsConn
			c.br = bufio.NewReaderSize(tlsConn, 16*1024)
			c.bw = bufio.NewWriterSize(tlsConn, 16*1024)
			continue
		case codeGSSENC:
			// 不支持 GSSAPI 加密，回 'N'
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
			switch key {
			case "user":
				c.user = val
			case "database":
				c.dbName = val
			}
		}
		break
	}
	if c.user == "" {
		c.user = "godb"
	}
	// 选库放在认证之后：未认证就告诉对方「这个库不存在」等于泄露库名。
	if err := c.selectDB(); err != nil {
		c.write(encodeError("FATAL", "3D000", err.Error()))
		return err
	}

	// 认证：需要密码时走 AuthenticationCleartextPassword，否则 trust 直接放行
	if c.srv.authEnabled() {
		if err := c.authenticate(); err != nil {
			c.srv.logf("PG 认证失败（用户 %q，来自 %s）: %v", c.user, c.nc.RemoteAddr(), err)
			return err
		}
	}

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

// authenticate 走一次「明文密码」认证。
//
// PostgreSQL 的流程是：服务端发 AuthenticationCleartextPassword（R=3），
// 客户端回 PasswordMessage（'p'），服务端再发 AuthenticationOk。
// 密码错误时直接回 FATAL 28P01 并断开 —— 不做重试，客户端会自己重连。
func (c *conn) authenticate() error {
	// 优先 SCRAM（不传明文、不传可离线爆破的 md5），其次 md5，最后明文。
	if c.srv.AuthScram != nil {
		if enc, ok := c.srv.AuthScram(c.user); ok && enc != "" {
			return c.authenticateScram(enc)
		}
	}
	if c.srv.AuthMD5 != nil {
		if v, ok := c.srv.AuthMD5(c.user); ok && v != "" {
			return c.authenticateMD5(v)
		}
	}
	if err := c.write(encodeAuthenticationCleartextPassword()); err != nil {
		return err
	}
	typ, body, err := c.readMsg()
	if err != nil {
		return err
	}
	if typ != 'p' {
		c.write(encodeError("FATAL", "08P01", "expected password message"))
		return fmt.Errorf("expected password message, got %q", typ)
	}
	pw := string(trimZero(body))
	if c.srv.AuthVerify == nil || !c.srv.AuthVerify(c.user, pw) {
		c.write(encodeError("FATAL", "28P01",
			fmt.Sprintf(`password authentication failed for user %q`, c.user)))
		return fmt.Errorf("password authentication failed")
	}
	return nil
}

// trimZero 去掉消息尾部的 0（PasswordMessage 是 null-terminated 字符串）。
func trimZero(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
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
		if !c.dispatch(typ, payload) {
			return
		}
	}
}

// dispatch 处理一条前端消息，返回 false 表示连接该结束了。
//
// 每条消息都套一层 recover：一个 bug 不能让整个进程退出（以前一条带表的 CTE
// 就能把库打死，所有连接一起断）。panic 之后把写缓冲清掉（里面可能留着半条
// 消息），回一个 XX000，然后照常等 Sync —— 客户端会看到一条普通报错。
func (c *conn) dispatch(typ byte, payload []byte) (keep bool) {
	defer func() {
		if r := recover(); r != nil {
			c.bw.Reset(c.nc)
			c.srv.logf("PG 连接内 panic（消息 %q）: %v", typ, r)
			_ = c.write(encodeError("ERROR", "XX000", fmt.Sprintf("internal error: %v", r)))
			c.skipSync = true
			keep = true
		}
	}()
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
		return false
	default:
		// 忽略不认识的类型（如 COPY 相关）
	}
	return true
}

// ---------- 简单查询 ----------

func (c *conn) handleSimpleQuery(payload []byte) {
	text := cstr(payload, 0)
	if strings.TrimSpace(text) == "" {
		_ = c.write(encodeEmptyQuery(), encodeReadyForQuery(c.txStatus()))
		return
	}
	c.srv.logQuery(text)
	// SET / SHOW 由 SQL 层处理（见 exec.ShowResult），这里不再拦一道，
	// 免得网页控制台和 PG 协议两条路径的行为分叉。
	// COPY 走自己的数据流（CopyIn/CopyOut），不能按普通查询处理
	if cp := parseCopyStatement(text); cp != nil {
		c.setActivity("active", text)
		c.handleCopy(cp)
		c.setActivity("idle", "")
		return
	}
	c.setActivity("active", text)
	results, err := c.db.Exec(c.sess, text)
	defer c.setActivity("idle", "")
	var out [][]byte
	for _, r := range results {
		out = append(out, encodeResult(r, true)...)
	}
	if err != nil {
		out = append(out, c.errorResponse(err)...)
	}
	out = append(out, encodeReadyForQuery(c.txStatus()))
	_ = c.write(out...)
}

// encodeResult 用全文本格式编码一条结果（简单查询用）。
func encodeResult(r *exec.Result, withRowDesc bool) [][]byte {
	out, err := encodeResultFormats(r, withRowDesc, nil)
	if err != nil {
		return nil
	}
	return out
}

// encodeResultFormats 按客户端在 Bind 里要求的格式编码结果行。
//
// formats 为空表示全文本；长度 1 表示所有列同一格式；否则逐列对应。
// 二进制编码失败时返回错误（调用方还没写出任何字节，可以干净地改成 ErrorResponse）——
// 半行二进制半行文本比报错更糟，客户端会一直读到错位的数据。
func encodeResultFormats(r *exec.Result, withRowDesc bool, formats []int16) ([][]byte, error) {
	var out [][]byte
	if len(r.Columns) > 0 {
		// 扩展协议里 RowDescription 已经由 Describe 发过一次了，Execute 只补 DataRow；
		// 再发一遍 Npgsql 会直接抛 "unexpected backend message of type RowDescription"。
		if withRowDesc {
			fields := make([]field, len(r.Columns))
			for i, col := range r.Columns {
				oid, size := typeOID(col.Type)
				fields[i] = field{
					Name: col.Name, TypeOID: oid, TypeLen: size,
					TableOID: col.TableOID, AttNum: col.AttNum,
				}
			}
			out = append(out, encodeRowDescription(fields))
		}
		oids := make([]int32, len(r.Columns))
		for i, col := range r.Columns {
			oids[i], _ = typeOID(col.Type)
		}
		// 逐列对应时数量必须一致：数量对不上说明客户端和我们的列数认知不同，
		// 这时候按文本硬发会让它把 4 字节当 2 字节读，干脆报错。
		if len(formats) > 1 && len(formats) != len(r.Columns) {
			return nil, fmt.Errorf("client requested %d result formats for %d columns",
				len(formats), len(r.Columns))
		}
		for _, row := range r.Rows {
			vals := make([][]byte, len(row))
			for i, v := range row {
				if resultFormatAt(formats, i) == 1 && i < len(oids) {
					b, err := encodeBinaryValue(v, oids[i])
					if err != nil {
						return nil, err
					}
					vals[i] = b
					continue
				}
				colType := ""
				if i < len(r.Columns) {
					colType = r.Columns[i].Type
				}
				vals[i] = textValueAs(v, colType)
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
	return out, nil
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
	// 预编译语句（扩展协议）也要进查询日志：客户端有的元数据查询走
	// Parse/Bind/Execute 而不是简单查询，不记下来排查时会完全看不见。
	c.srv.logQuery(query)
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
	// 结果格式：客户端要求文本还是二进制，逐列给。
	var resultFormats []int16
	if off+2 <= len(payload) {
		n := int(int16(binary.BigEndian.Uint16(payload[off:])))
		off += 2
		for i := 0; i < n && off+2 <= len(payload); i++ {
			resultFormats = append(resultFormats, int16(binary.BigEndian.Uint16(payload[off:])))
			off += 2
		}
	}
	c.portals[portalName] = &portal{stmt: stmt, sql: sqlText, resultFormats: resultFormats}
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
	if kind == 'P' && hasRows {
		// Describe(portal) 的 RowDescription 必须回**实际要发的格式**（Bind 里指定的）。
		// Postgres 就是这么做的：statement 一律报 text，portal 报 Bind 决定的格式。
		// 我们以前一律报 0，Npgsql 于是把二进制列当文本读 —— 后果是类型处理器
		// 退化成 UnknownTypeHandler（ResolveHandler 里 text 格式就不查类型表），
		// GetInt32 报 "Can't cast database type .<unknown>"。
		if p := c.portals[name]; p != nil {
			for i := range fields {
				fields[i].Format = resultFormatAt(p.resultFormats, i)
			}
		}
	}
	if !hasRows {
		out = append(out, encodeNoData())
	} else {
		out = append(out, encodeRowDescription(fields))
	}
	_ = c.write(out...)
}

// resultFormatAt 取第 i 列的结果格式：0=文本，1=二进制。
// 空表示全文本；1 个表示所有列同一格式；否则逐列对应。
func resultFormatAt(formats []int16, i int) int16 {
	switch {
	case len(formats) == 0:
		return 0
	case len(formats) == 1:
		return formats[0]
	case i < len(formats):
		return formats[i]
	}
	return 0
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
	results, err := c.db.Exec(c.sess, p.sql)
	var out [][]byte
	for _, r := range results {
		// false：Execute 阶段不再重复 RowDescription（Describe 已经发过），
		// 否则 Npgsql 等严格客户端会认为报文顺序非法。
		msgs, eerr := encodeResultFormats(r, false, p.resultFormats)
		if eerr != nil {
			// 一条行都还没发出去，可以整个换成错误响应
			out = nil
			err = eerr
			break
		}
		out = append(out, msgs...)
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
	sc, err := exec.DescribeWith(c.db.Cat, c.db.Engine.Info, stmts[len(stmts)-1])
	if err != nil || len(sc) == 0 {
		return nil, false
	}
	fields := make([]field, len(sc))
	for i, col := range sc {
		oid, size := typeOID(col.Type.String())
		fields[i] = field{
			Name: col.Name, TypeOID: oid, TypeLen: size,
			TableOID: col.TableOID, AttNum: col.AttNum,
		}
	}
	return fields, true
}

// ---------- 类型映射与文本编码 ----------

// typeOID 把我们的类型名映射成 PG 的 OID。
// typeOID 返回 PG 类型 OID 与 typlen，统一走 types 包，避免两处映射不一致。
func typeOID(t string) (int32, int16) {
	return types.OIDByName(t), int16(types.TypeLenByName(t))
}

// textValueAs 按列类型把结果值编码成 PG 文本格式。
//
// numeric 要用定点表示：内部是 float64，用 'g' 会把 1234567890123.45 输出成
// 1.23456789012345e+12，而 PG 的 numeric 从不用科学计数法 —— 客户端（Npgsql 的
// decimal / FreeSql 的 decimal.Parse）拿到指数形式会直接解析失败。
// float8 保持 'g'，PG 的大数值也是指数形式。
func textValueAs(v interface{}, colType string) []byte {
	lower := strings.ToLower(colType)
	// date 列只输出日期：内部时间值统一带 " HH:MM:SS"，PG 的 date 文本只有年月日
	if strings.HasPrefix(lower, "date") {
		if s, ok := v.(string); ok && len(s) >= 10 {
			return []byte(s[:10])
		}
	}
	if f, ok := v.(float64); ok && strings.HasPrefix(lower, "numeric") {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			// PG 的 numeric 支持 NaN，输出也是 NaN / Infinity
			return []byte(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return []byte(strconv.FormatFloat(f, 'f', -1, 64))
	}
	return textValue(v)
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

// parseCopyStatement 判断这条语句是不是单独的 COPY。不是就返回 nil（走普通路径）。
func parseCopyStatement(text string) *sql.Copy {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 4 || !strings.EqualFold(trimmed[:4], "copy") {
		return nil
	}
	stmts, err := sql.Parse(text)
	if err != nil || len(stmts) != 1 {
		return nil
	}
	if cp, ok := stmts[0].(*sql.Copy); ok {
		return cp
	}
	return nil
}

// handleCopy 执行 COPY FROM STDIN / TO STDOUT。
func (c *conn) handleCopy(cp *sql.Copy) {
	if cp.To {
		lines, n, err := c.db.CopyOut(c.sess, cp)
		if err != nil {
			_ = c.write(c.errorResponse(err)...)
			_ = c.write(encodeReadyForQuery(c.txStatus()))
			return
		}
		msgs := [][]byte{encodeCopyResponse(msgCopyOutResponse, 1)}
		for _, line := range lines {
			msgs = append(msgs, encodeCopyData([]byte(line+"\n")))
		}
		msgs = append(msgs, encodeCopyDone(),
			encodeCommandComplete(fmt.Sprintf("COPY %d", n)),
			encodeReadyForQuery(c.txStatus()))
		_ = c.write(msgs...)
		return
	}

	// COPY FROM STDIN：先告诉客户端可以发数据了
	if err := c.write(encodeCopyResponse(msgCopyInResponse, 1)); err != nil {
		return
	}
	var data []byte
	for {
		typ, body, err := c.readMsg()
		if err != nil {
			return
		}
		switch typ {
		case msgCopyData:
			data = append(data, body...)
		case msgCopyDone:
			n, err := c.db.CopyIn(c.sess, cp, data)
			if err != nil {
				_ = c.write(c.errorResponse(err)...)
				_ = c.write(encodeReadyForQuery(c.txStatus()))
				return
			}
			_ = c.write(encodeCommandComplete(fmt.Sprintf("COPY %d", n)),
				encodeReadyForQuery(c.txStatus()))
			return
		case msgCopyFail:
			_ = c.write(c.errorResponse(fmt.Errorf("COPY from stdin failed: %s", string(body)))...)
			_ = c.write(encodeReadyForQuery(c.txStatus()))
			return
		default:
			// Sync / Flush 之类：忽略
		}
	}
}
