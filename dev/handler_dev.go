//go:build dev

package dev

import (
	"fmt"
	"net/http"
)

// newHandler：dev 构建返回 DevServer 的 handler，fallback 忽略。
//
// 启动失败（例如 fsnotify 初始化失败）直接 panic：接线发生在启动阶段，
// 此时退化成 fallback 只会让人以为 dev server 在工作。
func newHandler(root string, fallback func() http.Handler, opts []Option) http.Handler {
	_ = fallback
	ds, err := NewDevServer(root, opts...)
	if err != nil {
		panic(fmt.Sprintf("webfront dev: 启动 %s 的 dev server 失败: %v", root, err))
	}
	return ds.Handler()
}
