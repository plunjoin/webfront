//go:build !dev

package dev

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 普通构建：Handler 必须原样返回 fallback，不能碰磁盘（root 不存在也无所谓）。
func TestHandlerUsesFallbackWithoutDevTag(t *testing.T) {
	called := false
	h := Handler(filepath.Join(t.TempDir(), "missing"), func() http.Handler {
		called = true
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	}, WithBasePath("/admin"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if !called {
		t.Fatal("普通构建应调用 fallback")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// fallback 缺失时给 404，而不是 panic 或 nil handler。
func TestHandlerNilFallback(t *testing.T) {
	for name, h := range map[string]http.Handler{
		"nil":        Handler("./web", nil),
		"nil result": Handler("./web", func() http.Handler { return nil }),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, http.StatusNotFound)
		}
	}
}
