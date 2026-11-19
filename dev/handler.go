package dev

import "net/http"

// Handler 是应用侧唯一的开发态接线点：调用它的文件不需要 build tag。
//
//	admin := dev.Handler("web/admin", webui.AdminHandler, dev.WithBasePath("/admin"))
//	portal := dev.Handler("web/portal", func() http.Handler { return webui.PortalHandler(db) })
//	mux.Handle("/admin/", admin)
//	mux.Handle("/", portal)
//
// 普通构建：直接返回 fallback()，不会碰磁盘、不会启动监听；fallback 为 nil
// 或返回 nil 时得到 404。dev 包的轻量文件只依赖标准库，esbuild / fsnotify /
// websocket 都在 //go:build dev 文件里，生产二进制不会带上它们。
//
// -tags dev：在 root（前端工程目录，不是 dist）上启动 DevServer 并返回它的
// handler，fallback 不会被调用。多个 root 各自调用一次 Handler 即可，选项与
// NewDevServer 完全一致（WithBasePath 用于子路径挂载）。
//
// fallback 必须是"构造器"而不是已经构造好的 handler：dev 构建里它不执行，
// 这样 newEngine() 之类依赖产物的初始化不会因为 dist 还没构建而失败。注意
// -tags dev 仍然会编译 fallback 所在的包；若该包用 //go:embed 嵌入 dist，
// 按 GUIDE §6.1 把 embed 声明放进 //go:build !dev 文件，并补一个 dev 构建
// 用的空 FS 桩。
func Handler(root string, fallback func() http.Handler, opts ...Option) http.Handler {
	return newHandler(root, fallback, opts)
}
