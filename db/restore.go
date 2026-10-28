package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"godb/catalog"
	"godb/storage"
)

// RestoreReport 是一次回收站恢复的结果。
type RestoreReport struct {
	// Entry 是被恢复的备份目录名。
	Entry string
	// At / Reason 来自 manifest，方便打印「恢复的是哪一次、什么原因删的」。
	At     string
	Reason string
	// Restored 是恢复成功的表名（附行数）。
	Restored []RestoredTable
	// Skipped 是同名表已存在、没有覆盖的表名。
	Skipped []string
	// Generation 是恢复时对齐到的 meta 代次。
	Generation int64
}

// RestoredTable 是恢复成功的一张表。
type RestoredTable struct {
	Name string
	Rows int
	// Bytes 是写入的数据文件大小（0 表示这张表本来就没有数据）。
	Bytes int64
}

// RestoreTrash 把回收站里的一次备份恢复回数据目录。
//
// entry 传备份目录名；传 "latest" 或空串表示恢复最新的一份。恢复只做「加回
// 不存在同名表的表」，已经存在的表跳过而不是覆盖 —— 恢复动作本身不能再变成
// 一次数据损失。
//
// 必须在 godb 服务停止时执行：直接改磁盘上的 catalog.json 和表文件，运行中的
// 进程内存里那份不会跟着变。
func RestoreTrash(root, entry string) (RestoreReport, error) {
	var rep RestoreReport
	dir, err := resolveTrashEntry(root, entry)
	if err != nil {
		return rep, err
	}
	mf, err := readTrashManifest(dir)
	if err != nil {
		return rep, err
	}
	rep.Entry, rep.At, rep.Reason = filepath.Base(dir), mf.At, mf.Reason

	gen, err := storage.MetaGeneration(root)
	if err != nil {
		return rep, err
	}
	rep.Generation = gen

	cat, err := catalog.Load(root)
	if err != nil {
		return rep, err
	}
	for _, def := range mf.Defs {
		if def == nil || def.Name == "" {
			continue
		}
		if _, exists := cat.Get(def.Name); exists {
			rep.Skipped = append(rep.Skipped, def.Name)
			continue
		}
		// 表 ID 决定数据文件名，可能和现在的某张表撞车，所以重新分配。
		// DROP 之后 ID 不复用是设计约定，恢复相当于一次新的建表。
		oldID := def.ID
		def.ID = 0
		if err := cat.Add(def); err != nil {
			return rep, fmt.Errorf("恢复表 %q 失败: %w", def.Name, err)
		}
		newID := def.ID
		rt := RestoredTable{Name: def.Name}
		if src := trashTableFile(dir, oldID); src != "" {
			data, err := os.ReadFile(src)
			if err != nil {
				return rep, err
			}
			n, err := storage.InstallTableSnap(root, newID, data, gen)
			if err != nil {
				return rep, fmt.Errorf("恢复表 %q 的数据失败: %w", def.Name, err)
			}
			rt.Bytes = n
			if rows, ok := trashRowsOf(mf, def.Name); ok {
				rt.Rows = rows
			}
		}
		rep.Restored = append(rep.Restored, rt)
	}
	if len(rep.Restored) > 0 {
		if err := cat.Save(root); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// trashRowsOf 从 manifest 里取某张表当时的行数（manifest 按名字索引）。
func trashRowsOf(mf storage.TrashManifest, name string) (int, bool) {
	for _, t := range mf.Tables {
		if t.Name == name {
			return t.Rows, true
		}
	}
	return 0, false
}

func readTrashManifest(dir string) (storage.TrashManifest, error) {
	var mf storage.TrashManifest
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return mf, fmt.Errorf("读取 %s 失败: %w", filepath.Join(dir, "manifest.json"), err)
	}
	if err := json.Unmarshal(b, &mf); err != nil {
		return mf, fmt.Errorf("解析 manifest.json 失败: %w", err)
	}
	if len(mf.Defs) == 0 {
		return mf, fmt.Errorf("备份里没有表定义，无法恢复: %s", dir)
	}
	return mf, nil
}

// resolveTrashEntry 把 "latest" / 空串 / 目录名 / 绝对路径 统一成备份目录。
func resolveTrashEntry(root, entry string) (string, error) {
	trashRoot := filepath.Join(root, storage.TrashDirName())
	if entry != "" && entry != "latest" {
		if filepath.IsAbs(entry) {
			if st, err := os.Stat(entry); err == nil && st.IsDir() {
				return entry, nil
			}
			return "", fmt.Errorf("备份目录不存在: %s", entry)
		}
		dir := filepath.Join(trashRoot, entry)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
		return "", fmt.Errorf("回收站里没有这个备份: %s", entry)
	}
	entries, err := storage.TrashEntries(root)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("回收站是空的（%s）", trashRoot)
	}
	return filepath.Join(trashRoot, entries[0]), nil
}

// trashTableFile 返回备份里某张表的数据文件路径；没有则返回空串。
func trashTableFile(dir string, id int64) string {
	if id <= 0 {
		return ""
	}
	p := filepath.Join(dir, "tables", fmt.Sprintf("t%06d.snp", id))
	if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
		return p
	}
	return ""
}
