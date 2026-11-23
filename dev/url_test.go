//go:build dev

package dev

import (
	"net/http"
	"strings"
	"testing"
)

// TestDevCanonicalModuleURL：扩展名/index 补全后必须 302 到真实文件 URL。
// 否则模块内部的相对 import 会以“目录”为 base（/src/lib/i18n + ./en
// 变成 /src/lib/en），HMR 的 owner URL 也会和实际模块 URL 不一致。
func TestDevCanonicalModuleURL(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/router.ts", `import "./pages/Home"; export const router = 1`)
	writeGraphFile(t, tmp, "src/pages/Home.ts", `export const home = 1`)
	writeGraphFile(t, tmp, "src/lib/i18n/index.ts", `import "./en"; export const i18n = 1`)
	writeGraphFile(t, tmp, "src/lib/i18n/en.ts", `export const en = 1`)

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := ds.Handler()

	cases := []struct {
		request string
		want    string
	}{
		{"/src/router", "/src/router.ts"},
		{"/src/router?t=7", "/src/router.ts?t=7"},
		{"/src/lib/i18n", "/src/lib/i18n/index.ts"},
		{"/src/lib/i18n/", "/src/lib/i18n/index.ts"},
		{"/src/lib/i18n?raw=1", "/src/lib/i18n/index.ts?raw=1"},
		// 文件 URL 上的多余尾斜杠同样要规范化：否则 ./pages 会解析成
		// /src/router.ts/pages。
		{"/src/router.ts/", "/src/router.ts"},
	}
	for _, c := range cases {
		rec := doGet(h, c.request)
		if rec.Code != http.StatusFound {
			t.Fatalf("GET %s = %d, want 302 (body=%q)", c.request, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != c.want {
			t.Fatalf("GET %s Location = %q, want %q", c.request, loc, c.want)
		}
	}

	// 真实文件本身不再重定向，且图里的 owner URL 是 canonical URL。
	rec := doGet(h, "/src/router.ts")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "router") {
		t.Fatalf("GET /src/router.ts = %d %q", rec.Code, rec.Body.String())
	}
	if node, ok := ds.Module("src/router.ts"); !ok || node.URL != "/src/router.ts" {
		t.Fatalf("router node = %+v, ok=%v", node, ok)
	}
	if _, ok := ds.Module("src/router"); ok {
		t.Fatal("不应为未规范化的 URL 建图节点")
	}
}

// TestDevCanonicalModuleURLBasePath：重定向目标必须带挂载前缀。
func TestDevCanonicalModuleURLBasePath(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/router.ts", `export const router = 1`)

	ds, err := NewDevServer(tmp, WithBasePath("/admin"))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	rec := doGet(ds.Handler(), "/admin/src/router?t=3")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/src/router.ts?t=3" {
		t.Fatalf("GET = %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}

	// ServeMux 自己的补斜杠 / clean-path 重定向也必须保留挂载前缀。
	for _, c := range []struct {
		request string
		want    string
	}{
		{"/admin/src", "/admin/src/"},
		{"/admin//src/router.ts", "/admin/src/router.ts"},
	} {
		rec := doGet(ds.Handler(), c.request)
		if rec.Code < 300 || rec.Code >= 400 {
			t.Fatalf("GET %s = %d，期望 3xx（Location=%q）", c.request, rec.Code, rec.Header().Get("Location"))
		}
		if loc := rec.Header().Get("Location"); loc != c.want {
			t.Fatalf("GET %s Location = %q，期望 %q", c.request, loc, c.want)
		}
	}
}

// TestDevCanonicalModuleURLBasePathStripPrefix：调用方按文档用
// http.StripPrefix 挂载时，ServeMux 的补斜杠 / clean-path 重定向仍需补回
// basePath；canonical 重定向本身已带前缀，不能被二次添加。
func TestDevCanonicalModuleURLBasePathStripPrefix(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/router.ts", `export const router = 1`)

	ds, err := NewDevServer(tmp, WithBasePath("/admin"))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := http.StripPrefix("/admin", ds.Handler())

	for _, c := range []struct {
		request string
		want    string
	}{
		{"/admin/src", "/admin/src/"},
		{"/admin//src/router.ts", "/admin/src/router.ts"},
	} {
		rec := doGet(h, c.request)
		if rec.Code < 300 || rec.Code >= 400 {
			t.Fatalf("GET %s = %d，期望 3xx（Location=%q）", c.request, rec.Code, rec.Header().Get("Location"))
		}
		if loc := rec.Header().Get("Location"); loc != c.want {
			t.Fatalf("GET %s Location = %q，期望 %q", c.request, loc, c.want)
		}
	}

	rec := doGet(h, "/admin/src/router")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/src/router.ts" {
		t.Fatalf("canonical GET = %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestDevPublicDirectoryRedirect：public 目录请求补尾斜杠，index.html 的
// 相对链接才能按目录解析；查询串保留。
func TestDevPublicDirectoryRedirect(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/main.tsx", `export const render = () => {}`)
	writePublicFile(t, tmp, "public/docs/index.html", "<html>docs</html>")

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := ds.Handler()

	rec := doGet(h, "/docs?from=test")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/docs/?from=test" {
		t.Fatalf("GET /docs = %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}
	rec = doGet(h, "/docs/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "docs") {
		t.Fatalf("GET /docs/ = %d %q", rec.Code, rec.Body.String())
	}
}
