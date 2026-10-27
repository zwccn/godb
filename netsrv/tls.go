package netsrv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"godb/config"
)

// loadTLS 按配置准备 TLS 证书。
//
// 三种情况：
//   - 没开 SSL           → nil（客户端请求 SSL 时回 'N'，回落明文）
//   - 配了证书与私钥文件 → 从文件加载
//   - 只开了开关         → 生成一张临时自签证书（进程内有效）
//
// 临时证书不落盘：自签证书本来就无法被客户端验证，落盘只会带来「过期 / 权限」
// 这类额外问题。真要对外服务，请配 sslCertFile / sslKeyFile。
func loadTLS(n config.Network) (*tls.Certificate, error) {
	if !n.SSLEnabled {
		return nil, nil
	}
	if n.SSLCertFile != "" || n.SSLKeyFile != "" {
		if n.SSLCertFile == "" || n.SSLKeyFile == "" {
			return nil, fmt.Errorf("sslCertFile 与 sslKeyFile 必须同时设置")
		}
		cert, err := tls.LoadX509KeyPair(n.SSLCertFile, n.SSLKeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载证书失败: %w", err)
		}
		return &cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "godb", Organization: []string{"godb"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}
