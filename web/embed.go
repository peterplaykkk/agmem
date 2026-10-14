// Package webui 内嵌 web/dist 构建产物（Vite SPA），以单二进制方式随 agmem 分发。
// 模式：//go:embed 只能引用本包目录内文件，故 embed 声明放在 web/ 目录下；
// cmd/agmem 用 webui.Handler(srv.Routes()) 包一层：/api/* 走后端，其余走 SPA。
//
// CSP 注意：前端使用 MUI/emotion 运行时注入 <style>，若上层网关
// 对页面下发严格 CSP，需允许 style-src 'unsafe-inline'（或按响应注入 nonce），
// 否则样式会被浏览器拦截。api 包的 default-src 'self' 允许内联样式不生效——
// emotion 走 <style> 注入，需 style-src 'unsafe-inline' 或 nonce 方案（待评审）。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler 返回静态资源处理器：非 /api/ 路径服务 dist 内文件，
// 未命中文件时回退 index.html（SPA 深链刷新），命中 /api/ 时透传给后端路由。
func Handler(next http.Handler) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return next
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r.URL.Path = "/" // SPA fallback：深链刷新返回 shell
		}
		fileServer.ServeHTTP(w, r)
	})
}
