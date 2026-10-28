package main

import (
	"os"
	"path/filepath"
	"testing"
)

// hasCatalog 是「启动时该不该自动载入示例数据」的判据：
// 只有目录里根本没有 catalog.json 才算新库。
func TestHasCatalog(t *testing.T) {
	dir := t.TempDir()
	if hasCatalog(dir) {
		t.Fatalf("空目录不该被当成已有库")
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(`{"tables":[]}`), 0o644); err != nil {
		t.Fatalf("写 catalog: %v", err)
	}
	if !hasCatalog(dir) {
		t.Fatalf("有 catalog.json（哪怕 0 张表）就该当成已有库，不能再自动补样例")
	}
}
