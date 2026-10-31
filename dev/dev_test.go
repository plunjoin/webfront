package dev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/plunjoin/webfront/compiler"
)

func TestDevServerImportMapAndDeps(t *testing.T) {
	tmp := t.TempDir()

	// 准备 .store/manifest.json
	storeDir := filepath.Join(tmp, "wf_modules", ".store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟 fake-dep
	pkgDir := filepath.Join(storeDir, "fake-dep@1.0.0")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"fake-dep","version":"1.0.0","module":"index.js"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.js"), []byte(`export const hello = "world";`), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := compiler.StoreManifest{
		Root: map[string]string{
			"fake-dep": "1.0.0",
		},
		Packages: map[string]map[string]string{},
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	handler := ds.Handler()

	// 1. 测试 GET / 返回 HTML 并包含 importmap
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<script type="importmap">`) {
		t.Fatalf("expected importmap tag in HTML: %s", body)
	}
	if !strings.Contains(body, `"/@deps/fake-dep.js"`) {
		t.Fatalf("expected /@deps/fake-dep.js in importmap: %s", body)
	}

	// 2. 测试 GET /@deps/fake-dep.js 成功打包并返回 ESM
	reqDep := httptest.NewRequest(http.MethodGet, "/@deps/fake-dep.js", nil)
	wDep := httptest.NewRecorder()
	handler.ServeHTTP(wDep, reqDep)
	if wDep.Code != http.StatusOK {
		t.Fatalf("expected 200 for /@deps/fake-dep.js, got %d: %s", wDep.Code, wDep.Body.String())
	}
	depBody := wDep.Body.String()
	if !strings.Contains(depBody, `hello`) {
		t.Fatalf("expected bundled code in /@deps/fake-dep.js, got: %s", depBody)
	}

	// 3. 再次请求测试缓存命中
	wDep2 := httptest.NewRecorder()
	handler.ServeHTTP(wDep2, reqDep)
	if wDep2.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached dep, got %d", wDep2.Code)
	}
}

// TestDevIndexClientScript 保证注入浏览器的 HMR 脚本本身是合法 JS，
// 并且带上了"后端重启后自动重载"的能力（wf dev 依赖它）。
func TestDevIndexClientScript(t *testing.T) {
	ds := &DevServer{entry: "/src/main.tsx", rootDeps: map[string]string{}}
	h := ds.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	html := rec.Body.String()

	const open = "<script>\n"
	start := strings.Index(html, open)
	if start < 0 {
		t.Fatalf("index 里找不到内联脚本:\n%s", html)
	}
	rest := html[start+len(open):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatalf("内联脚本没有闭合:\n%s", html)
	}
	script := rest[:end]

	// 用 esbuild 当 JS 解析器：模板字符串拼错会在这里暴露，而不是等到浏览器白屏。
	if res := api.Transform(script, api.TransformOptions{Loader: api.LoaderJS}); len(res.Errors) > 0 {
		t.Fatalf("HMR 客户端脚本有语法错误: %+v\n%s", res.Errors, script)
	}
	for _, want := range []string{"/__wf_ping", "location.reload()", "showOverlay", "wss://"} {
		if !strings.Contains(script, want) {
			t.Fatalf("HMR 脚本缺少 %q", want)
		}
	}

	// 探活端点必须够轻：204 + 不缓存
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, httptest.NewRequest(http.MethodGet, "/__wf_ping", nil))
	if pr.Code != http.StatusNoContent {
		t.Fatalf("/__wf_ping 期望 204，实际 %d", pr.Code)
	}
	if got := pr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("/__wf_ping Cache-Control = %q，期望 no-store", got)
	}
}
