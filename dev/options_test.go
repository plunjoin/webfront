//go:build dev

package dev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plunjoin/webfront/compiler"
)

func doGet(h http.Handler, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// writeStore 在 modulesDir 下造一个最小版本化 store。
// pkgs: 包名 -> 包内 index.js 内容。
func writeStore(t *testing.T, modulesDir string, rootDeps map[string]string, pkgs map[string]string) {
	t.Helper()
	store := filepath.Join(modulesDir, ".store")
	for name, code := range pkgs {
		dir := filepath.Join(store, compiler.StoreDirName(name, "1.0.0"))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		pkg := map[string]any{"name": name, "version": "1.0.0", "module": "index.js"}
		pb, _ := json.Marshal(pkg)
		if err := os.WriteFile(filepath.Join(dir, "package.json"), pb, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := compiler.StoreManifest{Root: rootDeps, Packages: map[string]map[string]string{}}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDevBasePath 验证壳 HTML、import map、HMR 地址都带挂载前缀，
// 且直接挂 /admin/ 与外部 StripPrefix 两种方式都能服务。
func TestDevBasePath(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "src", "main.tsx"),
		[]byte(`export const render = () => {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStore(t, filepath.Join(tmp, "wf_modules"), map[string]string{"fake-dep": "1.0.0"}, map[string]string{
		"fake-dep": `export const hello = "world"`,
	})

	ds, err := NewDevServer(tmp, WithBasePath("/admin/"))
	if err != nil {
		t.Fatal(err)
	}
	h := ds.Handler()

	rec := doGet(h, "/admin/")
	if rec.Code != 200 {
		t.Fatalf("GET /admin/ = %d: %s", rec.Code, rec.Body.String())
	}
	html := rec.Body.String()
	for _, want := range []string{
		`"/admin/@deps/fake-dep.js"`,
		`var BASE = "/admin"`,
		`"/admin/src/main.tsx"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index 缺少 %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `"/@deps/fake-dep.js"`) {
		t.Errorf("basePath 下不应再出现根路径 import map:\n%s", html)
	}

	// 直接挂在 /admin/ 下：路径带着前缀进来。
	if rec := doGet(h, "/admin/src/main.tsx"); rec.Code != 200 {
		t.Fatalf("GET /admin/src/main.tsx = %d: %s", rec.Code, rec.Body.String())
	}
	// 调用方自己 StripPrefix 后：路径已是内部形态。
	if rec := doGet(h, "/src/main.tsx"); rec.Code != 200 {
		t.Fatalf("GET /src/main.tsx（已剥前缀）= %d: %s", rec.Code, rec.Body.String())
	}
	// HMR 广播的 path 与 import map 用同一套前缀。
	if u, ok := ds.devURL(filepath.Join(tmp, "src", "main.tsx")); !ok || u != "/admin/src/main.tsx" {
		t.Fatalf("devURL = %q, %v", u, ok)
	}
}

// TestDevModulesDirUpward：前端工程把共享 store 放在上级目录时，
// dev server 应自动找到 ../wf_modules，而不是退到 node_modules。
func TestDevModulesDirUpward(t *testing.T) {
	tmp := t.TempDir()
	shared := filepath.Join(tmp, "wf_modules")
	writeStore(t, shared, map[string]string{"fake-dep": "1.0.0"}, map[string]string{
		"fake-dep": `export const hello = "world"`,
	})
	app := filepath.Join(tmp, "admin")
	if err := os.MkdirAll(filepath.Join(app, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "src", "main.tsx"), []byte(`export const render = () => {}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := NewDevServer(app)
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(ds.Handler(), "/")
	if !strings.Contains(rec.Body.String(), `"/@deps/fake-dep.js"`) {
		t.Fatalf("没有找到上级 wf_modules 的依赖:\n%s", rec.Body.String())
	}
	if rec := doGet(ds.Handler(), "/@deps/fake-dep.js"); rec.Code != 200 {
		t.Fatalf("GET /@deps/fake-dep.js = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDevExternalSingleton：root 依赖之间默认外置，react-dom 不会内联一份 react，
// 而是留下裸导入交给 import map，运行时只有一份实例。
func TestDevExternalSingleton(t *testing.T) {
	tmp := t.TempDir()
	writeStore(t, filepath.Join(tmp, "wf_modules"),
		map[string]string{"fake-react": "1.0.0", "fake-react-dom": "1.0.0"},
		map[string]string{
			"fake-react": `export const MARK = "REACT_RUNTIME_SOURCE"; export function hello() { return MARK }`,
			// CJS 依赖用 require() 取 react：外置后必须变成静态 import，
			// 不能留下浏览器跑不了的 __require()。
			"fake-react-dom": `const React = require("fake-react"); module.exports = { hi: React.hello }`,
		})

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(ds.Handler(), "/@deps/fake-react-dom.js")
	if rec.Code != 200 {
		t.Fatalf("GET /@deps/fake-react-dom.js = %d: %s", rec.Code, rec.Body.String())
	}
	code := rec.Body.String()
	if !strings.Contains(code, `import * as __wf_ext_ns_0 from "fake-react";`) {
		t.Fatalf("fake-react 没有被外置成静态导入:\n%s", code)
	}
	if strings.Contains(code, `__require("fake-react")`) {
		t.Fatalf("外置后仍留下浏览器跑不了的 __require():\n%s", code)
	}
	if strings.Contains(code, "REACT_RUNTIME_SOURCE") {
		t.Fatalf("react runtime 被内联进了 react-dom（会出现双实例）:\n%s", code)
	}

	// 显式关闭自动外置时恢复内联。
	ds2, err := NewDevServer(tmp, WithExternal())
	if err != nil {
		t.Fatal(err)
	}
	rec = doGet(ds2.Handler(), "/@deps/fake-react-dom.js")
	if !strings.Contains(rec.Body.String(), "REACT_RUNTIME_SOURCE") {
		t.Fatalf("WithExternal() 应关闭自动外置:\n%s", rec.Body.String())
	}
}

// TestDevDefine：Vue 特性开关自动注入，WithDefine 追加的项生效且优先。
func TestDevDefine(t *testing.T) {
	tmp := t.TempDir()
	writeStore(t, filepath.Join(tmp, "wf_modules"),
		map[string]string{"vue": "1.0.0", "fake-dep": "1.0.0"},
		map[string]string{
			"vue":      `export const flags = [__VUE_OPTIONS_API__, process.env.NODE_ENV, __APP_MODE__]`,
			"fake-dep": `export const mode = __APP_MODE__`,
		})

	ds, err := NewDevServer(tmp, WithDefine(map[string]string{"__APP_MODE__": `"custom"`}))
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(ds.Handler(), "/@deps/vue.js")
	if rec.Code != 200 {
		t.Fatalf("GET /@deps/vue.js = %d: %s", rec.Code, rec.Body.String())
	}
	code := rec.Body.String()
	for _, want := range []string{"true", "development", "custom"} {
		if !strings.Contains(code, want) {
			t.Errorf("define 后产物缺少 %q:\n%s", want, code)
		}
	}
	if strings.Contains(code, "__VUE_OPTIONS_API__") || strings.Contains(code, "__APP_MODE__") {
		t.Fatalf("define 没有替换裸全局:\n%s", code)
	}
}

// TestDevDepsCSSSubpath：nprogress/nprogress.css 这类子路径必须产出
// "导入即注入 <style>" 的 ESM，而不是当 JS 入口打包报错。
func TestDevDepsCSSSubpath(t *testing.T) {
	tmp := t.TempDir()
	store := filepath.Join(tmp, "wf_modules")
	writeStore(t, store, map[string]string{"fake-css": "1.0.0"}, map[string]string{
		"fake-css": `export const x = 1`,
	})
	if err := os.WriteFile(filepath.Join(store, ".store", "fake-css@1.0.0", "style.css"), []byte("body{color:red}"), 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(ds.Handler(), "/@deps/fake-css/style.css")
	if rec.Code != 200 {
		t.Fatalf("GET /@deps/fake-css/style.css = %d: %s", rec.Code, rec.Body.String())
	}
	code := rec.Body.String()
	if !strings.Contains(code, "document.createElement('style')") ||
		!strings.Contains(code, "body") || !strings.Contains(code, "color: red") {
		t.Fatalf("CSS 子路径没有包成注入样式的 ESM:\n%s", code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type = %q", ct)
	}
}
