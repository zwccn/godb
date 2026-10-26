package web

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assetsFS embed.FS

// Assets 返回内嵌的前端静态文件系统。
func Assets() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}
