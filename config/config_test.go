package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCreatesDefaultFile(t *testing.T) {
	dir := t.TempDir()
	st, err := Load(dir, Default(), nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("配置文件应该被创建: %v", err)
	}
	c := st.Get()
	if c.Storage.CheckpointCommits != 128 {
		t.Fatalf("默认提交阈值 = %d, want 128", c.Storage.CheckpointCommits)
	}
	if c.Storage.CheckpointSeconds != 60 || c.Storage.CheckpointWALMB != 64 {
		t.Fatalf("默认时间/WAL 阈值不对: %+v", c.Storage)
	}
	if c.Network.HTTPRemote || c.Network.PGRemote {
		t.Fatal("默认不允许远程访问")
	}
	if c.Auth.RequirePassword {
		t.Fatal("默认不启用密码")
	}
}

func TestSavePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	st, err := Load(dir, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := st.Get()
	c.Storage.CheckpointCommits = 7
	c.Network.HTTPPort = 9091
	c.Network.HTTPRemote = true
	saved, err := st.Save(c)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.Network.HTTPPort != 9091 {
		t.Fatalf("返回值不对: %+v", saved.Network)
	}

	// 重新加载（模拟重启）
	st2, err := Load(dir, Default(), []string{"-http"})
	if err != nil {
		t.Fatal(err)
	}
	c2 := st2.Get()
	if c2.Storage.CheckpointCommits != 7 || c2.Network.HTTPPort != 9091 || !c2.Network.HTTPRemote {
		t.Fatalf("重新加载后配置不对: %+v", c2)
	}
	if len(st2.IgnoredFlags) != 1 || st2.IgnoredFlags[0] != "-http" {
		t.Fatalf("应该记录被忽略的命令行参数: %v", st2.IgnoredFlags)
	}
}

func TestSaveValidates(t *testing.T) {
	dir := t.TempDir()
	st, err := Load(dir, Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := st.Get()

	cases := []struct {
		name string
		mu   func(*Config)
		want string
	}{
		{"端口越界", func(c *Config) { c.Network.HTTPPort = 0 }, "HTTP 端口"},
		{"端口相同", func(c *Config) { c.Network.PGEnabled = true; c.Network.PGPort = c.Network.HTTPPort }, "不能和 HTTP 端口相同"},
		{"负数提交", func(c *Config) { c.Storage.CheckpointCommits = -1 }, "不能为负数"},
		{"秒数越界", func(c *Config) { c.Storage.CheckpointSeconds = 999999 }, "0 到 86400"},
		{"密码无哈希", func(c *Config) { c.Auth.RequirePassword = true; c.Auth.PasswordHash = "" }, "必须设置密码"},
		{"密码无用户", func(c *Config) {
			c.Auth.RequirePassword = true
			c.Auth.User = "  "
			c.Auth.PasswordHash = "pbkdf2-sha256$1$aaaa$bbbb"
		}, "必须填写用户名"},
	}
	for _, tc := range cases {
		c := base
		tc.mu(&c)
		err := c.Validate()
		if err == nil {
			t.Fatalf("%s: 应该报错", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: 错误信息 %q 不含 %q", tc.name, err.Error(), tc.want)
		}
	}

	// 校验失败时不能把坏配置写进文件
	bad := base
	bad.Network.HTTPPort = 0
	if _, err := st.Save(bad); err == nil {
		t.Fatal("Save 应该拒绝非法配置")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(raw), `"httpPort": 0`) {
		t.Fatal("非法配置不应该写进文件")
	}
}

func TestLoadRejectsBrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, Default(), nil); err == nil {
		t.Fatal("坏配置文件应该报错，而不是静默用默认值覆盖")
	}
}

func TestSeedFromFlags(t *testing.T) {
	c := Seed("0.0.0.0:9090", "127.0.0.1:7000", -1)
	if c.Network.HTTPPort != 9090 || !c.Network.HTTPRemote {
		t.Fatalf("HTTP 种子不对: %+v", c.Network)
	}
	if c.Network.PGPort != 7000 || c.Network.PGRemote {
		t.Fatalf("PG 种子不对: %+v", c.Network)
	}

	c = Seed("127.0.0.1:8080", "", -1)
	if c.Network.PGEnabled {
		t.Fatal("-pg 为空时应该禁用 PG 协议")
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	h, err := HashPassword("s3cret-密码")
	if err != nil {
		t.Fatal(err)
	}
	if !LooksHashed(h) {
		t.Fatalf("格式不对: %s", h)
	}
	if strings.Contains(h, "s3cret") {
		t.Fatal("哈希里不应该出现明文")
	}
	if !VerifyPassword(h, "s3cret-密码") {
		t.Fatal("正确密码应该通过")
	}
	if VerifyPassword(h, "s3cret-密码2") {
		t.Fatal("错误密码不应该通过")
	}
	// 损坏的编码串要返回 false，而不是 panic
	for _, bad := range []string{"", "abc", "pbkdf2-sha256$0$aa$bb", "pbkdf2-sha256$x$aa$bb", "md5$1$aa$bb"} {
		if VerifyPassword(bad, "whatever") {
			t.Fatalf("损坏的哈希 %q 不应该通过", bad)
		}
	}
}

func TestHTTPAddr(t *testing.T) {
	n := Network{HTTPPort: 8080}
	if got := n.HTTPAddr(); got != "127.0.0.1:8080" {
		t.Fatalf("仅本机 = %s", got)
	}
	n.HTTPRemote = true
	if got := n.HTTPAddr(); got != "0.0.0.0:8080" {
		t.Fatalf("允许远程 = %s", got)
	}
	n.PGEnabled = false
	if got := n.PGAddr(); got != "" {
		t.Fatalf("未启用时 PGAddr 应为空，得到 %s", got)
	}
}
