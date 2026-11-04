package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// .trash/vec 里的向量文件必须有自己的清理策略。
//
// 它不在 trashKeep 的计数里（而且 'v' > '2'，降序永远排最前面），
// 所以 pruneTrash 永远轮不到它 —— 实测跑几轮 DROP 就攒了 9.8MB 且只增不减。
func TestPruneTrashVecKeepsBounded(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	vecDir := filepath.Join(dir, trashDirName, vecTrashDir)
	if err := os.MkdirAll(vecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 造 trashVecKeep+5 个文件，mtime 从旧到新
	base := time.Now().Add(-time.Hour)
	for i := 0; i < trashVecKeep+5; i++ {
		p := filepath.Join(vecDir, fmt.Sprintf("t%06d.c001.vec", i))
		if err := os.WriteFile(p, make([]byte, 64), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	// 一个备份目录：不能被误删
	backup := filepath.Join(dir, trashDirName, "20260101-000000-drop-table")
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}

	s.pruneTrash()

	ents, err := os.ReadDir(vecDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != trashVecKeep {
		t.Fatalf(".trash/vec 应保留 %d 个，实际 %d", trashVecKeep, len(ents))
	}
	// 留下的必须是最新的那批（最老的 5 个被删）
	for _, e := range ents {
		var idx int
		if _, err := fmt.Sscanf(e.Name(), "t%06d.c001.vec", &idx); err != nil {
			t.Fatalf("文件名解析失败: %s", e.Name())
		}
		if idx < 5 {
			t.Fatalf("最旧的 t%06d 应该被删掉，却还在", idx)
		}
	}
	// 备份目录不受影响
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("备份目录被误删: %v", err)
	}
}
