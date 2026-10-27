package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// md5 挑战应答：服务端只有 md5(md5(pw+user))，能算出正确的应答。
func TestMD5Challenge(t *testing.T) {
	v := MD5Verifier("godb", "s3cret")
	if len(v) != 32 {
		t.Fatalf("md5 verifier 长度 = %d，期望 32", len(v))
	}
	// 同样的输入必须稳定，换个用户必须不同
	if MD5Verifier("godb", "s3cret") != v {
		t.Fatal("同样的输入应该得到同样的 verifier")
	}
	if MD5Verifier("other", "s3cret") == v {
		t.Fatal("换了用户名 verifier 应该不同")
	}

	salt := []byte{1, 2, 3, 4}
	resp := MD5Response(v, salt)
	if !strings.HasPrefix(resp, "md5") || len(resp) != 35 {
		t.Fatalf("应答格式不对: %q", resp)
	}
	// 不同盐 → 不同应答
	if MD5Response(v, []byte{9, 9, 9, 9}) == resp {
		t.Fatal("不同盐应该得到不同应答")
	}
}

// SCRAM：完整跑一遍客户端侧计算，服务端校验必须通过；改一位就必须失败。
func TestScramRoundTrip(t *testing.T) {
	enc, err := ScramVerifier("s3cret")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if !LooksScram(enc) {
		t.Fatalf("验证串格式不对: %q", enc)
	}
	iter, salt, storedKey, serverKey, ok := ScramKeys(enc)
	if !ok {
		t.Fatal("验证串解析失败")
	}
	if iter != 4096 || len(salt) != 16 || len(storedKey) != 32 || len(serverKey) != 32 {
		t.Fatalf("参数不对: iter=%d salt=%d stored=%d server=%d", iter, len(salt), len(storedKey), len(serverKey))
	}

	// --- 模拟客户端 ---
	clientNonce := "clientnonce123"
	clientFirstBare := "n=godb,r=" + clientNonce
	serverFirst := "r=" + clientNonce + "serversuffix,s=" + base64.StdEncoding.EncodeToString(salt) +
		",i=4096"
	clientFinalNoProof := "c=biws,r=" + clientNonce + "serversuffix"
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof

	salted := pbkdf2SHA256([]byte("s3cret"), salt, iter, 32)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	clientSig := hmacSHA256(sha256Sum(clientKey), []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSig[i]
	}

	sig, ok := ScramCheckProof(storedKey, serverKey, authMessage, proof)
	if !ok {
		t.Fatal("正确的证明应该通过")
	}
	// 服务器签名必须等于客户端自己算的 ServerSignature
	wantSig := hmacSHA256(serverKey, []byte(authMessage))
	if !bytes.Equal(sig, wantSig) {
		t.Fatal("服务器签名不一致")
	}

	// 篡改一位就要失败
	bad := make([]byte, len(proof))
	copy(bad, proof)
	bad[0] ^= 0xff
	if _, ok := ScramCheckProof(storedKey, serverKey, authMessage, bad); ok {
		t.Fatal("错误的证明不该通过")
	}
	// 口令不同也不行
	enc2, _ := ScramVerifier("wrong")
	_, _, s2, v2, _ := ScramKeys(enc2)
	if _, ok := ScramCheckProof(s2, v2, authMessage, proof); ok {
		t.Fatal("别的口令不该通过")
	}
}

// 验证串损坏时不能放行，也不能 panic。
func TestScramBadVerifier(t *testing.T) {
	for _, bad := range []string{"", "SCRAM-SHA-256$abc", "SCRAM-SHA-256$4096$xx$yy$zz", "MD5$1$2$3$4"} {
		if _, _, _, _, ok := ScramKeys(bad); ok {
			t.Fatalf("%q 不该解析成功", bad)
		}
	}
}

// 盐必须每次不同（否则同一密码的验证串会一样）。
func TestScramSaltIsRandom(t *testing.T) {
	a, err := ScramVerifier("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ScramVerifier("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次生成的验证串不该相同（盐应当随机）")
	}
}

// 顺带确认随机源可用（避免被 mock 掉时静默全零）。
func TestSaltNotAllZero(t *testing.T) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Fatal("随机源返回了全零")
	}
}
