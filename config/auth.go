package config

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// 这个文件放「客户端认证」需要的派生凭据。
//
// 为什么不能只用 PBKDF2 哈希：PostgreSQL 的 md5 与 SCRAM 都不发送明文密码，
// 客户端与服务端各算一半，服务端必须事先存好对应的派生值：
//
//	md5     存 md5(md5(password+user))，用于挑战应答
//	SCRAM   存 SaltedPassword 派生出的 StoredKey / ServerKey
//
// 三者都从同一个明文密码算出，改密码时一起重算。明文与 PBKDF2 串都不外泄。

const (
	// scramIterations 是 SCRAM 的迭代次数（PostgreSQL 默认 4096）
	scramIterations = 4096
	// scramPrefix 是验证串的前缀
	scramPrefix = "SCRAM-SHA-256"
)

// ---------- md5 ----------

// MD5Verifier 返回 md5(md5(password+user)) 的十六进制摘要。
//
// 它与 PostgreSQL 自己存的 pg_authid.rolpassword 里的形式一致（那串以 "md5"
// 开头，这里只存后面 32 位十六进制，前缀在拼应答时加）。
func MD5Verifier(user, password string) string {
	h := md5.Sum([]byte(password + user))
	return hex.EncodeToString(h[:])
}

// MD5Response 计算期望的挑战应答：md5(verifier + salt) 再拼 "md5" 前缀。
func MD5Response(verifier string, salt []byte) string {
	h := md5.New()
	h.Write([]byte(verifier))
	h.Write(salt)
	return "md5" + hex.EncodeToString(h.Sum(nil))
}

// ---------- SCRAM-SHA-256 ----------

// hmacSHA256 是 SCRAM 各处的 HMAC。
func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// sha256Sum 是 SCRAM 里的 H()。
func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// ScramVerifier 生成 "SCRAM-SHA-256$迭代$盐$StoredKey$ServerKey" 形式的验证串
// （各段 base64）。和 PostgreSQL 的 pg_authid 存法一致。
func ScramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	salted := pbkdf2SHA256([]byte(password), salt, scramIterations, 32)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	storedKey := sha256Sum(clientKey)
	enc := base64.StdEncoding
	return fmt.Sprintf("%s$%d$%s$%s$%s", scramPrefix, scramIterations,
		enc.EncodeToString(salt),
		enc.EncodeToString(storedKey),
		enc.EncodeToString(serverKey)), nil
}

// ScramKeys 从验证串里取出校验所需的三样东西。
func ScramKeys(encoded string) (iter int, salt, storedKey, serverKey []byte, ok bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != scramPrefix {
		return 0, nil, nil, nil, false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return 0, nil, nil, nil, false
	}
	enc := base64.StdEncoding
	salt, err = enc.DecodeString(parts[2])
	if err != nil {
		return 0, nil, nil, nil, false
	}
	storedKey, err = enc.DecodeString(parts[3])
	if err != nil {
		return 0, nil, nil, nil, false
	}
	serverKey, err = enc.DecodeString(parts[4])
	if err != nil {
		return 0, nil, nil, nil, false
	}
	return iter, salt, storedKey, serverKey, true
}

// ScramCheckProof 校验客户端的 ClientProof，成功时返回需要回给客户端的
// ServerSignature（v=... 后面的 base64）。
//
// authMessage = client-first-bare + "," + server-first + "," + client-final-without-proof
func ScramCheckProof(storedKey, serverKey []byte, authMessage string, proof []byte) ([]byte, bool) {
	clientSignature := hmacSHA256(storedKey, []byte(authMessage))
	if len(proof) != len(clientSignature) {
		return nil, false
	}
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ clientSignature[i]
	}
	if subtle.ConstantTimeCompare(sha256Sum(clientKey), storedKey) != 1 {
		return nil, false
	}
	return hmacSHA256(serverKey, []byte(authMessage)), true
}

// LooksScram 判断字符串是不是 SCRAM 验证串。
func LooksScram(s string) bool { return strings.HasPrefix(s, scramPrefix+"$") }
