// Package frontend 嵌入前端构建产物（frontend/dist）。
package frontend

import "embed"

//go:embed all:dist
var FS embed.FS
