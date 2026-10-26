package pgwire

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"godb/db"
	"godb/lock"
)

// startServerWithAuth 起一个要求密码的 PG 监听。
func startServerWithAuth(t *testing.T) (*db.DB, string) {
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
	if _, err := d.Exec(d.Session("init"), `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{
		DB:           d,
		Logf:         func(string, ...interface{}) {},
		AuthRequired: func() bool { return true },
		AuthVerify: func(user, password string) bool {
			return user == "godb" && password == "s3cret"
		},
	}
	go srv.Serve(ln)
	t.Cleanup(func() { ln.Close() })
	return d, ln.Addr().String()
}

// handshake 走完启动握手。返回是否被要求输入密码、错误文本、是否成功。
func handshake(t *testing.T, addr, user, password string) (askedPassword bool, errText string, ok bool) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))

	body := binary.BigEndian.AppendUint32(nil, protoVersion3)
	body = append(body, []byte("user\x00"+user+"\x00")...)
	body = append(body, 0)
	startup := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	startup = append(startup, body...)
	if _, err := conn.Write(startup); err != nil {
		t.Fatalf("write startup: %v", err)
	}

	for i := 0; i < 40; i++ {
		typ, payload, err := readOne(conn)
		if err != nil {
			return askedPassword, "read: " + err.Error(), false
		}
		switch typ {
		case 'R':
			if len(payload) < 4 {
				return askedPassword, "short auth message", false
			}
			code := binary.BigEndian.Uint32(payload[:4])
			if code == 3 {
				askedPassword = true
				if _, err := conn.Write(pgMsg('p', tstr(password))); err != nil {
					return askedPassword, "write password: " + err.Error(), false
				}
			}
		case 'E':
			return askedPassword, errTextField(payload), false
		case 'Z':
			return askedPassword, "", true
		}
	}
	return askedPassword, "没有等到 ReadyForQuery", false
}

func readOne(conn net.Conn) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:]))
	if n < 4 || n > 1<<20 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

// errTextField 从 ErrorResponse 里取出人类可读的那段（字段 'M'）。
func errTextField(payload []byte) string {
	for i := 0; i < len(payload); {
		code := payload[i]
		i++
		j := i
		for j < len(payload) && payload[j] != 0 {
			j++
		}
		if code == 'M' {
			return string(payload[i:j])
		}
		i = j + 1
	}
	return string(payload)
}

func TestAuthRejectsWrongPassword(t *testing.T) {
	_, addr := startServerWithAuth(t)
	asked, errText, ok := handshake(t, addr, "godb", "wrong")
	if !asked {
		t.Fatal("应该先收到 AuthenticationCleartextPassword")
	}
	if ok {
		t.Fatal("错误密码不应该通过")
	}
	if !strings.Contains(errText, "password authentication failed") {
		t.Fatalf("错误信息不对: %q", errText)
	}
}

func TestAuthRejectsWrongUser(t *testing.T) {
	_, addr := startServerWithAuth(t)
	asked, errText, ok := handshake(t, addr, "someone", "s3cret")
	if !asked || ok {
		t.Fatalf("错误用户名不应该通过（asked=%v ok=%v）", asked, ok)
	}
	if !strings.Contains(errText, "password authentication failed") {
		t.Fatalf("错误信息不对: %q", errText)
	}
}

func TestAuthAcceptsCorrectPassword(t *testing.T) {
	_, addr := startServerWithAuth(t)
	asked, errText, ok := handshake(t, addr, "godb", "s3cret")
	if !asked {
		t.Fatal("应该要求密码")
	}
	if !ok {
		t.Fatalf("正确密码应该通过: %s", errText)
	}
}

// 认证通过之后必须还能正常跑 SQL —— 别只测握手。
func TestAuthThenQuery(t *testing.T) {
	_, addr := startServerWithAuth(t)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))

	body := binary.BigEndian.AppendUint32(nil, protoVersion3)
	body = append(body, []byte("user\x00godb\x00")...)
	body = append(body, 0)
	startup := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	conn.Write(append(startup, body...))

	// 读到要求密码 → 回密码 → 读到 ReadyForQuery
	for {
		typ, payload, err := readOne(conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == 'E' {
			t.Fatalf("认证失败: %s", errTextField(payload))
		}
		if typ != 'R' {
			continue
		}
		if binary.BigEndian.Uint32(payload[:4]) == 3 {
			if _, err := conn.Write(pgMsg('p', tstr("s3cret"))); err != nil {
				t.Fatal(err)
			}
		}
		break
	}
	for {
		typ, _, err := readOne(conn)
		if err != nil {
			t.Fatalf("read ready: %v", err)
		}
		if typ == 'Z' {
			break
		}
	}

	// 简单查询
	if _, err := conn.Write(pgMsg('Q', append([]byte("INSERT INTO t VALUES (1,'x')"), 0))); err != nil {
		t.Fatal(err)
	}
	for {
		typ, payload, err := readOne(conn)
		if err != nil {
			t.Fatalf("read insert: %v", err)
		}
		if typ == 'E' {
			t.Fatalf("INSERT 失败: %s", errTextField(payload))
		}
		if typ == 'Z' {
			break
		}
	}
}

// 没配密码时仍然是 trust，客户端不应该被要求输入密码。
func TestTrustWhenAuthDisabled(t *testing.T) {
	_, addr := startServer(t)
	asked, errText, ok := handshake(t, addr, "zwccn", "")
	if asked {
		t.Fatal("未启用密码时不该要求输入密码")
	}
	if !ok {
		t.Fatalf("trust 模式应该直接连上: %s", errText)
	}
}
