// Package web 内嵌构建后的前端静态资源,使最终产物是单个二进制文件。
package web

import "embed"

// Dist 是 Vite 的构建产物(web/dist)。未执行前端构建时目录里只有
// .gitkeep 占位,服务端会退化为一页构建指引,go build 始终可用。
//
//go:embed all:dist
var Dist embed.FS
