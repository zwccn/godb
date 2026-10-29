package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 崩在「重写完成、rename 之前」：两个 .new 都在，标记也在 → 启动时补做完。
func TestFinishPendingSwapsCompletes(t *testing.T) {
	dir := t.TempDir()
	tables := filepath.Join(dir, "tables")
	if err := os.MkdirAll(tables, 0o755); err != nil {
		t.Fatal(err)
	}
	dat := filepath.Join(tables, "t000001.dat")
	lob := filepath.Join(tables, "t000001.lob")
	// 旧内容
	mustWrite(t, dat, "OLD-DAT")
	mustWrite(t, lob, "OLD-LOB")
	// 新内容 + 标记（相当于「写完 .new 并 fsync，刚写标记就崩了」）
	mustWrite(t, dat+".new", "NEW-DAT")
	mustWrite(t, lob+".new", "NEW-LOB")
	mustWrite(t, dat+".swap", swapMarkerMagic)

	finishPendingSwaps(dir)

	if got := mustRead(t, dat); got != "NEW-DAT" {
		t.Fatalf("页文件还是 %q，rename 没补做", got)
	}
	if got := mustRead(t, lob); got != "NEW-LOB" {
		t.Fatalf("大字段还是 %q，rename 没补做", got)
	}
	if _, err := os.Stat(dat + ".swap"); !os.IsNotExist(err) {
		t.Fatal("标记应该被删掉")
	}
	for _, p := range []string{dat + ".new", lob + ".new"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应该已经被 rename 走", p)
		}
	}
}

// 崩在「写标记之前」：.new 是垃圾，启动时清掉，旧文件保持不动。
func TestFinishPendingSwapsDropsStale(t *testing.T) {
	dir := t.TempDir()
	tables := filepath.Join(dir, "tables")
	if err := os.MkdirAll(tables, 0o755); err != nil {
		t.Fatal(err)
	}
	dat := filepath.Join(tables, "t000002.dat")
	mustWrite(t, dat, "GOOD")
	mustWrite(t, dat+".new", "HALF-WRITTEN")
	mustWrite(t, filepath.Join(tables, "t000002.lob.new"), "HALF-LOB")

	finishPendingSwaps(dir)

	if got := mustRead(t, dat); got != "GOOD" {
		t.Fatalf("旧文件被动了：%q", got)
	}
	for _, p := range []string{dat + ".new", filepath.Join(tables, "t000002.lob.new")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应该被清掉", p)
		}
	}
}

func mustWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
