//go:build !dev

package dev

import "net/http"

// newHandler：生产构建没有 DevServer，直接把 fallback() 交回去。
// root / opts 只在 dev 构建里有意义，这里刻意不碰它们。
func newHandler(root string, fallback func() http.Handler, opts []Option) http.Handler {
	if fallback == nil {
		return http.NotFoundHandler()
	}
	if h := fallback(); h != nil {
		return h
	}
	return http.NotFoundHandler()
}
