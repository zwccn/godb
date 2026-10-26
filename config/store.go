package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FileName 是配置文件名（放在数据目录下）。
const FileName = "config.json"

// ErrReadOnly 表示调用方不允许写配置（例如远程访问设置页）。
// 放在这里是为了让 Web 层与调用方共用同一个错误文本。
var ErrReadOnly = fmt.Errorf("配置只能在本机上修改（当前请求来自非本机地址）")

// Store 是配置的内存副本 + 磁盘持久化。
//
// 并发安全：读写都加锁。Apply 的顺序由调用方控制（先写文件，
// 再让各个子系统生效；子系统失败时调用方回滚）。
type Store struct {
	path string

	mu  sync.RWMutex
	cur Config
	// IgnoredFlags 记录被配置文件覆盖掉的命令行参数，启动时打印出来，
	// 避免「我明明改了 -http 怎么没生效」的困惑。
	IgnoredFlags []string
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Get 返回当前配置的副本（含密码哈希，仅供内部使用，不要直接序列化给前端）。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Load 读取配置；文件不存在时用 seed 生成一份并落盘。
//
// seedFlags 是本次启动真正用了的命令行参数名（如 "http"、"pg"），
// 只在文件已存在时填进 IgnoredFlags，用来提示「以文件为准」。
func Load(dir string, seed Config, seedFlags []string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName)}

	b, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		var c Config
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("配置文件 %s 解析失败: %w（可以删掉它恢复默认配置）", s.path, err)
		}
		c.Normalize()
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("配置文件 %s 不合法: %w", s.path, err)
		}
		s.cur = c
		s.IgnoredFlags = append(s.IgnoredFlags, seedFlags...)
		return s, nil
	case os.IsNotExist(err):
		seed.Normalize()
		if err := seed.Validate(); err != nil {
			return nil, fmt.Errorf("初始配置不合法: %w", err)
		}
		if err := writeAtomic(s.path, seed); err != nil {
			return nil, err
		}
		s.cur = seed
		return s, nil
	default:
		return nil, err
	}
}

// Save 校验、落盘、更新内存。返回落盘后的配置。
//
// 顺序很重要：先写文件再更新内存，写失败时内存仍是旧值，
// 调用方可以安全地认为「保存失败 = 什么都没变」。
func (s *Store) Save(c Config) (Config, error) {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	if err := writeAtomic(s.path, c); err != nil {
		return Config{}, fmt.Errorf("写入配置文件失败: %w", err)
	}
	s.mu.Lock()
	s.cur = c
	s.mu.Unlock()
	return c, nil
}

// writeAtomic 用 tmp + fsync + rename 写文件，避免半截配置。
func writeAtomic(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---------- 密码哈希（PBKDF2-HMAC-SHA256，仅用标准库） ----------

const (
	pbkdf2Iter   = 120000
	pbkdf2KeyLen = 32
	pbkdf2Prefix = "pbkdf2-sha256"
)

// HashPassword 生成 "pbkdf2-sha256$迭代次数$盐$哈希" 形式的编码串。
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(password), salt, pbkdf2Iter, pbkdf2KeyLen)
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword 校验密码。编码串损坏时返回 false（不是 panic）。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// LooksHashed 判断字符串是不是本包生成的编码格式。
func LooksHashed(s string) bool {
	if !strings.HasPrefix(s, pbkdf2Prefix+"$") {
		return false
	}
	return len(strings.Split(s, "$")) == 4
}

// pbkdf2SHA256 是 RFC 2898 的 PBKDF2，PRF 固定为 HMAC-SHA256。
// Go 标准库没有暴露它（在 golang.org/x/crypto 里），这里自己实现，
// 避免引入任何第三方依赖。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	dk := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	var buf [4]byte
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dk = prf.Sum(dk)
		t := dk[len(dk)-hashLen:]
		copy(u, t)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = u[:0]
			u = prf.Sum(u)
			for i := range u {
				t[i] ^= u[i]
			}
		}
	}
	return dk[:keyLen]
}
