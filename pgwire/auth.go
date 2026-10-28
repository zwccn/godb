package pgwire

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"godb/config"
)

// ---------- md5 挑战应答 ----------
//
// 流程：服务端发 AuthenticationMD5Password（R=5，带 4 字节盐），
// 客户端回 'p' + "md5" + md5(md5(password+user) + salt)。
// 服务端只存 md5(password+user)，不存明文，也不需要它。

func encodeAuthenticationMD5Password(salt []byte) []byte {
	w := &writer{}
	w.int32(5)
	w.bytes(salt)
	return w.message(msgAuthentication)
}

func (c *conn) authenticateMD5(verifier string) error {
	salt := make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if err := c.write(encodeAuthenticationMD5Password(salt)); err != nil {
		return err
	}
	typ, body, err := c.readMsg()
	if err != nil {
		return err
	}
	if typ != 'p' {
		c.write(encodeErrorNoPos("FATAL", "08P01", "expected password message"))
		return fmt.Errorf("expected password message, got %q", typ)
	}
	got := string(trimZero(body))
	want := config.MD5Response(verifier, salt)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		c.write(encodeErrorNoPos("FATAL", "28P01",
			fmt.Sprintf(`password authentication failed for user %q`, c.user)))
		return fmt.Errorf("password authentication failed")
	}
	return nil
}

// ---------- SCRAM-SHA-256 ----------
//
// 服务端只需要存 StoredKey / ServerKey，全程不接触明文密码：
//
//	C -> S  SASLInitialResponse: "n,,n=user,r=<clientNonce>"
//	S -> C  AuthenticationSASLContinue(11): "r=<client><server>,s=<salt>,i=<iter>"
//	C -> S  SASLResponse: "c=biws,r=<combined>,p=<proof>"
//	S -> C  AuthenticationSASLFinal(12): "v=<serverSignature>"

func encodeAuthenticationSASL(mechs []string) []byte {
	w := &writer{}
	w.int32(10)
	for _, m := range mechs {
		w.str(m)
	}
	w.byte(0) // 机制列表结束
	return w.message(msgAuthentication)
}

func encodeAuthenticationSASLContinue(data string) []byte {
	w := &writer{}
	w.int32(11)
	w.bytes([]byte(data))
	return w.message(msgAuthentication)
}

func encodeAuthenticationSASLFinal(data string) []byte {
	w := &writer{}
	w.int32(12)
	w.bytes([]byte(data))
	return w.message(msgAuthentication)
}

func (c *conn) authenticateScram(verifier string) error {
	iter, salt, storedKey, serverKey, ok := config.ScramKeys(verifier)
	if !ok {
		// 验证串坏了：不要静默放行，也别让客户端卡住
		c.write(encodeErrorNoPos("FATAL", "28P01", "password verifier is corrupted"))
		return fmt.Errorf("bad scram verifier")
	}
	if err := c.write(encodeAuthenticationSASL([]string{"SCRAM-SHA-256"})); err != nil {
		return err
	}

	// --- 客户端首包 ---
	typ, body, err := c.readMsg()
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("expected SASLInitialResponse, got %q", typ)
	}
	mech, initial, err := parseSASLInitial(body)
	if err != nil {
		return err
	}
	if mech != "SCRAM-SHA-256" {
		c.write(encodeErrorNoPos("FATAL", "08P01", "unsupported SASL mechanism"))
		return fmt.Errorf("unsupported SASL mechanism %q", mech)
	}
	clientFirst := string(initial)
	// "n,,n=user,r=nonce" 里的 gs2 header 是固定两个逗号之前的部分
	i := strings.Index(clientFirst, ",,")
	if i < 0 {
		return fmt.Errorf("malformed SCRAM client-first message")
	}
	clientFirstBare := clientFirst[i+2:]
	clientNonce := scramAttr(clientFirstBare, "r")
	if clientNonce == "" {
		return fmt.Errorf("SCRAM client-first message has no nonce")
	}

	// --- 服务端首包 ---
	snRaw := make([]byte, 18)
	if _, err := rand.Read(snRaw); err != nil {
		return err
	}
	serverNonce := base64.StdEncoding.EncodeToString(snRaw)
	combined := clientNonce + serverNonce
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combined,
		base64.StdEncoding.EncodeToString(salt), iter)
	if err := c.write(encodeAuthenticationSASLContinue(serverFirst)); err != nil {
		return err
	}

	// --- 客户端末包 ---
	typ, body, err = c.readMsg()
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("expected SASLResponse, got %q", typ)
	}
	clientFinal := string(body)
	proofB64 := scramAttr(clientFinal, "p")
	if proofB64 == "" {
		return fmt.Errorf("SCRAM client-final message has no proof")
	}
	// client-final-without-proof 是认证串的一部分：去掉 ",p=..."
	pAt := strings.LastIndex(clientFinal, ",p=")
	if pAt < 0 {
		return fmt.Errorf("malformed SCRAM client-final message")
	}
	clientFinalNoProof := clientFinal[:pAt]
	if scramAttr(clientFinalNoProof, "r") != combined {
		c.write(encodeErrorNoPos("FATAL", "28P01", "SCRAM nonce mismatch"))
		return fmt.Errorf("SCRAM nonce mismatch")
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil {
		return fmt.Errorf("bad SCRAM proof encoding")
	}

	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof
	sig, ok := config.ScramCheckProof(storedKey, serverKey, authMessage, proof)
	if !ok {
		c.write(encodeErrorNoPos("FATAL", "28P01",
			fmt.Sprintf(`password authentication failed for user %q`, c.user)))
		return fmt.Errorf("password authentication failed")
	}
	return c.write(encodeAuthenticationSASLFinal("v=" + base64.StdEncoding.EncodeToString(sig)))
}

// parseSASLInitial 拆 SASLInitialResponse：机制名 + 初始响应。
func parseSASLInitial(body []byte) (mech string, initial []byte, err error) {
	end := -1
	for i, b := range body {
		if b == 0 {
			end = i
			break
		}
	}
	if end < 0 || end+5 > len(body) {
		return "", nil, fmt.Errorf("malformed SASLInitialResponse")
	}
	mech = string(body[:end])
	n := int(body[end+1])<<24 | int(body[end+2])<<16 | int(body[end+3])<<8 | int(body[end+4])
	start := end + 5
	if n < 0 || start+n > len(body) {
		return "", nil, fmt.Errorf("malformed SASLInitialResponse length")
	}
	return mech, body[start : start+n], nil
}

// scramAttr 从 "a=1,b=2" 里取某个属性值。
func scramAttr(msg, key string) string {
	for _, part := range strings.Split(msg, ",") {
		if strings.HasPrefix(part, key+"=") {
			return part[len(key)+1:]
		}
	}
	return ""
}
