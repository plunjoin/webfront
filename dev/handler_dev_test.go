//go:build dev

package dev

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -tags dev：Handler 返回 DevServer，fallback 不能被调用。
func TestHandlerStartsDevServerWithDevTag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.tsx"), []byte("document.title='x'"), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	h := Handler(root, func() http.Handler {
		called = true
		return http.NotFoundHandler()
	}, WithBasePath("/admin"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if called {
		t.Fatal("dev 构建不应调用 fallback")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`src="/admin/@wf/overlay.js"`,
		`type="module" src="/admin/@wf/boot"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dev 外壳缺少 %q:\n%s", want, body)
		}
	}

	// 入口与 BASE 现在固化在外部 boot / runtime 模块里。
	boot := httptest.NewRecorder()
	h.ServeHTTP(boot, httptest.NewRequest(http.MethodGet, "/admin/@wf/boot", nil))
	for _, want := range []string{
		`"/admin/src/main.tsx"`,
		`"/admin/@wf/client"`,
	} {
		if !strings.Contains(boot.Body.String(), want) {
			t.Fatalf("boot 缺少 %q:\n%s", want, boot.Body.String())
		}
	}
	client := httptest.NewRecorder()
	h.ServeHTTP(client, httptest.NewRequest(http.MethodGet, "/admin/@wf/client", nil))
	for _, want := range []string{
		`const BASE = "/admin";`,
		"BASE + '/__wf_hmr'",
	} {
		if !strings.Contains(client.Body.String(), want) {
			t.Fatalf("runtime 缺少 %q:\n%s", want, client.Body.String())
		}
	}
}
