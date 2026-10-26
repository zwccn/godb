package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const catalogFile = "catalog.json"

// Load 从目录读取 catalog.json；文件不存在时返回空 Catalog。
func Load(dir string) (*Catalog, error) {
	path := filepath.Join(dir, catalogFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	c.EnsureIDs()
	c.Rebuild()
	return &c, nil
}

// Save 原子写入 catalog.json（tmp + rename + fsync 目录项）。
func (c *Catalog) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, catalogFile)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
