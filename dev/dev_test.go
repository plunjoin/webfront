package dev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	// 覆盖层（Vite 同款）+ 重启探活 + 嵌套依赖错误回溯，一样都不能少。
	for _, want := range []string{
		"__wfOverlay", "attachShadow", "unhandledrejection",
		"/__wf_ping", "/__wf_error", "location.reload()", "wss://",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("HMR 脚本缺少 %q", want)
		}
	}
	// 覆盖层样式必须内联进 Shadow DOM，否则会被宿主页面的 CSS 改掉。
	if !strings.Contains(script, ".wf-ov-panel") {
		t.Fatal("覆盖层样式没有随脚本注入")
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

// TestDevServeSrcErrorIs500 固化另一条契约：编译错误必须变成 500 + 可读文本，
// 而不是 panic（dev server 与编译器同进程，panic 会直接带走整个进程）。
func TestDevServeSrcErrorIs500(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1) esbuild 侧：语法错误要带代码框（覆盖层原样展示）
	if err := os.WriteFile(filepath.Join(tmp, "src", "Bad.tsx"),
		[]byte(`export default function App() { return <div> }`), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	ds.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/src/Bad.tsx", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Bad.tsx") || !strings.Contains(body, "X [ERROR]") {
		t.Fatalf("响应体应是带代码框的错误原文，实际: %s", body)
	}

	// 2) .vue 侧：即使编译链缺依赖（这里没有 @vue/compiler-sfc），
	//    也只能是 500 + 说明，绝不能 panic 掉整个 dev 进程。
	if err := os.WriteFile(filepath.Join(tmp, "src", "Bad.vue"),
		[]byte(`<template><div/></template>`), 0o644); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	ds.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/src/Bad.vue", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf(".vue 期望 500，实际 %d: %s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Fatal(".vue 失败时响应体不能为空（覆盖层要展示它）")
	}
}

// TestDevErrorEndpoint 固化"嵌套依赖报错"的救援路径：
// 浏览器只会说 "Failed to fetch dynamically imported module"，真正的错误
// 靠 /__wf_error 取回。since 之后没有新错误时返回 204，避免覆盖层反复弹。
func TestDevErrorEndpoint(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 语法错误的 tsx：esbuild 会给出带代码框的报文
	bad := `export default function App() { return <div> }`
	if err := os.WriteFile(filepath.Join(tmp, "src", "App.tsx"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	h := ds.Handler()

	// 还没出错 → 204
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__wf_error", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("没有错误时应 204，实际 %d", rec.Code)
	}

	// 触发一次编译错误
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/src/App.tsx", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("坏文件应 500，实际 %d", rec.Code)
	}

	// 取回错误原文
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__wf_error?since=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("有错误时应 200，实际 %d", rec.Code)
	}
	var ce struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ce); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if ce.Path != "/src/App.tsx" {
		t.Errorf("path = %q", ce.Path)
	}
	if !strings.Contains(ce.Body, "App.tsx") {
		t.Errorf("body 里应有文件名: %s", ce.Body)
	}
	if ce.ID <= 0 {
		t.Errorf("id 必须递增且为正，实际 %d", ce.ID)
	}

	// 已经看过这条 → 204（客户端不会重复弹同一条）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__wf_error?since="+strconv.FormatInt(ce.ID, 10), nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("since 已是最新时应 204，实际 %d", rec.Code)
	}
}

// 浏览器不做扩展名补全：`import App from './App'` 会原样请求 /src/App。
// 构建期由 esbuild 解析，dev 期必须自己补，否则 HMR 链路一遇到相对导入就 404。
func TestDevServeSrcResolvesExtension(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(src, "widgets"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"App.tsx":              "export default function App() { return <div>hi</div> }\n",
		"widgets/index.ts":     "export const w = 1\n",
		"widgets/Card.vue.txt": "x",
		"data.json":            `{"a":1}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	h := ds.Handler()

	cases := []struct {
		url     string
		code    int
		contain string
	}{
		{"/src/App", http.StatusOK, "App"},          // 无扩展名 → App.tsx
		{"/src/App.tsx", http.StatusOK, "App"},      // 带扩展名照旧
		{"/src/widgets", http.StatusOK, "w"},        // 目录 → widgets/index.ts
		{"/src/data.json", http.StatusOK, "export"}, // JSON → ESM
		{"/src/Nope", http.StatusNotFound, "模块不存在"}, // 真的不存在才 404
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		if rec.Code != c.code {
			t.Errorf("GET %s = %d，期望 %d: %s", c.url, rec.Code, c.code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), c.contain) {
			t.Errorf("GET %s 响应体缺少 %q: %s", c.url, c.contain, rec.Body.String())
		}
	}
}
