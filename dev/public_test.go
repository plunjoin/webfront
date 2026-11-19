//go:build dev

package dev

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePublicFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDevPublicDir：public/ 是开发态静态资源目录，未命中内部路由的请求回退到这里；
// 内部路由优先，根路径仍是 HMR 外壳（public/index.html 顶不掉开发入口）。
func TestDevPublicDir(t *testing.T) {
	tmp := t.TempDir()
	writePublicFile(t, tmp, "src/main.tsx", `export const render = () => {}`)
	writePublicFile(t, tmp, "public/brand/logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)
	writePublicFile(t, tmp, "public/players/p1.json", `{"id":1}`)
	writePublicFile(t, tmp, "public/docs/index.html", "<html>docs</html>")
	writePublicFile(t, tmp, "public/index.html", "<html>public index</html>")
	writePublicFile(t, tmp, "public/src/main.tsx", "SHOULD NOT WIN")

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	h := ds.Handler()

	// 1) 命中 public 文件：内容 / MIME / dev 缓存策略
	rec := doGet(h, "/brand/logo.svg")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("GET /brand/logo.svg = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "svg") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if rec := doGet(h, "/players/p1.json"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"id":1}` {
		t.Fatalf("GET /players/p1.json = %d: %q", rec.Code, rec.Body.String())
	}

	// 2) 目录请求回目录下的 index.html（与 Vite 的 publicDir 一致）
	if rec := doGet(h, "/docs/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docs") {
		t.Fatalf("GET /docs/ = %d: %s", rec.Code, rec.Body.String())
	}

	// 3) 内部路由优先：public/src 不参与 /src 的按需转译
	if rec := doGet(h, "/src/main.tsx"); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "SHOULD NOT WIN") {
		t.Fatalf("/src/main.tsx 不应被 public 覆盖: %d %s", rec.Code, rec.Body.String())
	}

	// 4) 根路径永远是 HMR 外壳
	rec = doGet(h, "/")
	if !strings.Contains(rec.Body.String(), "webfront dev (HMR)") || strings.Contains(rec.Body.String(), "public index") {
		t.Fatalf("GET / 应是 dev 外壳: %s", rec.Body.String())
	}

	// 5) public 里没有的路径仍回 SPA 外壳
	if rec := doGet(h, "/nope/deep"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "webfront dev (HMR)") {
		t.Fatalf("未命中应回 SPA 外壳: %d %s", rec.Code, rec.Body.String())
	}
}

// TestDevPublicDirBasePath：public 文件与其它内部路由一样，跟着挂载前缀走。
func TestDevPublicDirBasePath(t *testing.T) {
	tmp := t.TempDir()
	writePublicFile(t, tmp, "public/brand/logo.svg", "<svg/>")

	ds, err := NewDevServer(tmp, WithBasePath("/admin"))
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(ds.Handler(), "/admin/brand/logo.svg")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("GET /admin/brand/logo.svg = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevPublicDirOption：WithPublicDir 可改目录，传空串关闭托管。
func TestDevPublicDirOption(t *testing.T) {
	tmp := t.TempDir()
	writePublicFile(t, tmp, "static/hello.txt", "hello")
	writePublicFile(t, tmp, "public/brand/logo.svg", "<svg/>")

	ds, err := NewDevServer(tmp, WithPublicDir("static"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := doGet(ds.Handler(), "/hello.txt"); rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("GET /hello.txt = %d: %q", rec.Code, rec.Body.String())
	}
	// 换成 static/ 后，默认的 public/ 不再托管
	if rec := doGet(ds.Handler(), "/brand/logo.svg"); !strings.Contains(rec.Body.String(), "webfront dev (HMR)") {
		t.Fatalf("public/ 不应再被托管: %s", rec.Body.String())
	}

	ds2, err := NewDevServer(tmp, WithPublicDir(""))
	if err != nil {
		t.Fatal(err)
	}
	if rec := doGet(ds2.Handler(), "/hello.txt"); !strings.Contains(rec.Body.String(), "webfront dev (HMR)") {
		t.Fatalf("WithPublicDir(\"\") 应关闭托管: %s", rec.Body.String())
	}
}
