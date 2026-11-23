package webfront

// webfront_test.go：核心运行时的库级测试，全部只依赖标准库。
//
// 关键场景是"Gin NoRoute 兜底"：Gin 在调用兜底处理器**之前**就把响应状态预设成
// 404（gin.serveError 里 c.writermem.status = 404），首个 Write 会把这个预设值
// 落盘。这里用 preset404Writer 复刻该语义（不 import Gin，保持库零框架依赖），
// 钉死"渲染成功 → 状态码必须 200"这条契约。

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// ---------------------------------------------------------------------------
// 测试基建
// ---------------------------------------------------------------------------

// preset404Writer 模拟 Gin 的 responseWriter 在 NoRoute 下的行为：
// 初始状态 404；WriteHeader(code) 在未落盘前可以覆盖状态；首个 Write 落盘状态。
type preset404Writer struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func newPreset404Writer() *preset404Writer {
	return &preset404Writer{hdr: http.Header{}, status: http.StatusNotFound}
}

func (w *preset404Writer) Header() http.Header { return w.hdr }

func (w *preset404Writer) WriteHeader(code int) {
	if w.wrote {
		return // gin：headers 落盘后再改状态只打 debug 警告
	}
	if code > 0 && w.status != code {
		w.status = code
	}
}

func (w *preset404Writer) Write(p []byte) (int, error) {
	w.wrote = true
	return w.body.Write(p)
}

func (w *preset404Writer) result() (int, string, http.Header) {
	return w.status, w.body.String(), w.hdr
}

// fakeRenderer 换掉 goja：RenderHTML 直接返回预制片段，测引擎层契约用。
type fakeRenderer struct{ html, head string }

func (f *fakeRenderer) RenderHTML(url string, propsJSON []byte) (string, string, error) {
	return f.html, f.head, nil
}
func (f *fakeRenderer) Close() error { return nil }

func distMapFS() fstest.MapFS {
	return fstest.MapFS{
		"dist/pages/index.html":       {Data: []byte("<!doctype html><html><body>prerendered-index<script type=\"module\" src=\"/assets/client.js\"></script></body></html>")},
		"dist/pages/about.html":       {Data: []byte("<!doctype html><html><body>prerendered-about<script type=\"module\" src=\"/assets/client.js\"></script></body></html>")},
		"dist/assets/client.js":       {Data: []byte("console.log('client')")},
		"dist/assets/chunks/chunk.js": {Data: []byte("console.log('chunk')")},
		"dist/index.html":             {Data: []byte("<!doctype html><html><body><div id=\"root\"></div><script type=\"module\" src=\"/assets/client.js\"></script></body></html>")},
	}
}

func staticEngine(t *testing.T, extra ...func(*Config)) *Engine {
	t.Helper()
	cfg := Config{Mode: ModeSSR, EmbedFS: distMapFS(), DistDir: "dist"}
	for _, fn := range extra {
		fn(&cfg)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func serveReq(h http.Handler, method, target string, hdr map[string]string) *preset404Writer {
	w := newPreset404Writer()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	h.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// 问题 1 的回归钉：Gin NoRoute 预设 404 之下，命中必须回 200
// ---------------------------------------------------------------------------

func TestNoRoutePreset404_PrerenderedPageReturns200(t *testing.T) {
	e := staticEngine(t)
	for _, path := range []string{"/", "/about"} {
		code, body, _ := serveReq(e, http.MethodGet, path, nil).result()
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200（NoRoute 预设的 404 必须被覆盖）", path, code)
		}
		want := "prerendered-index"
		if path == "/about" {
			want = "prerendered-about"
		}
		if !strings.Contains(body, want) {
			t.Errorf("GET %s 内容缺失 %q", path, want)
		}
		if !strings.Contains(body, `<script type="module" src="/assets/client.js">`) {
			t.Errorf("GET %s 缺少客户端脚本注入", path)
		}
	}
}

func TestNoRoutePreset404_MissStays404(t *testing.T) {
	e := staticEngine(t)
	code, body, _ := serveReq(e, http.MethodGet, "/nope", nil).result()
	if code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", code)
	}
	if !strings.Contains(body, "/nope") {
		t.Errorf("404 提示应包含请求路径, got %q", body)
	}
}

func TestNoRoutePreset404_AssetsReturn200(t *testing.T) {
	e := staticEngine(t)

	code, _, hdr := serveReq(e.StaticHandler(), http.MethodGet, "/assets/client.js", nil).result()
	if code != http.StatusOK {
		t.Fatalf("GET /assets/client.js = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("缺 ETag")
	}

	code, body, _ := serveReq(e.StaticHandler(), http.MethodGet, "/assets/client.js",
		map[string]string{"If-None-Match": etag}).result()
	if code != http.StatusNotModified {
		t.Errorf("条件请求 = %d, want 304", code)
	}
	if body != "" {
		t.Errorf("304 带了 %d 字节 body", len(body))
	}
}

func TestNoRoutePreset404_DynamicRenderReturns200(t *testing.T) {
	e := staticEngine(t, func(c *Config) {
		c.Renderer = &fakeRenderer{
			html: `<div id="root"><h1>ssr</h1></div>`,
			head: `<title>fake</title>`,
		}
	})
	w := newPreset404Writer()
	req := httptest.NewRequest(http.MethodGet, "/live", nil)
	if err := e.Render(w, req, Page{URL: "/live", Props: map[string]any{"title": "t"}}); err != nil {
		t.Fatal(err)
	}
	code, body, hdr := w.result()
	if code != http.StatusOK {
		t.Errorf("动态 Render = %d, want 200", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, must := range []string{
		"<!doctype html>",
		"<title>fake</title>",
		`id="__WF_DATA__"`,
		`<div id="root"><h1>ssr</h1></div>`,
		`<script type="module" src="/assets/client.js"></script>`,
	} {
		if !strings.Contains(body, must) {
			t.Errorf("动态文档缺少 %q", must)
		}
	}
}

// ---------------------------------------------------------------------------
// ModeSPA：命中文件直出，未命中回 index.html（History 路由 fallback）
// ---------------------------------------------------------------------------

func TestSPAModeServesShellAndAssets(t *testing.T) {
	e := staticEngine(t, func(c *Config) { c.Mode = ModeSPA })

	// 未命中路径 → SPA 外壳
	code, body, _ := serveReq(e, http.MethodGet, "/users/42", nil).result()
	if code != http.StatusOK {
		t.Errorf("SPA fallback = %d, want 200", code)
	}
	if !strings.Contains(body, `<div id="root"></div>`) {
		t.Errorf("SPA 外壳内容不对: %q", body)
	}

	// 命中文件 → 直出
	code, body, _ = serveReq(e, http.MethodGet, "/assets/chunks/chunk.js", nil).result()
	if code != http.StatusOK || !strings.Contains(body, "chunk") {
		t.Errorf("SPA 静态资源 = %d %q", code, body)
	}
}

// ---------------------------------------------------------------------------
// BasePath：前缀剥离、资源前缀注入、越界拒绝
// ---------------------------------------------------------------------------

// captureRenderer 记录引擎传给 SSR 入口的模板名和 Props。
type captureRenderer struct {
	url   string
	props string
	html  string
	head  string
}

func (c *captureRenderer) RenderHTML(url string, propsJSON []byte) (string, string, error) {
	c.url = url
	c.props = string(propsJSON)
	return c.html, c.head, nil
}
func (c *captureRenderer) Close() error { return nil }

func TestRenderTemplate(t *testing.T) {
	r := &captureRenderer{html: `<div id="root">hi</div>`, head: "<title>T</title>"}
	e, err := New(Config{Mode: ModeSSR, Renderer: r, ClientScript: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	doc, err := e.RenderTemplate("Home", struct {
		Title string `json:"title"`
	}{Title: "首页"})
	if err != nil {
		t.Fatal(err)
	}
	if r.url != "Home" {
		t.Fatalf("模板名 = %q, want Home", r.url)
	}
	if !strings.Contains(r.props, `"title":"首页"`) {
		t.Fatalf("Props 没带上 struct 字段: %s", r.props)
	}
	if !strings.Contains(doc, "<title>T</title>") || !strings.Contains(doc, `<div id="root">hi</div>`) {
		t.Fatalf("HTML 拼装不对: %s", doc)
	}
}

func TestRenderTemplateTo(t *testing.T) {
	r := &captureRenderer{html: `<div id="root">hi</div>`}
	e, err := New(Config{Mode: ModeSSR, Renderer: r, ClientScript: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := e.RenderTemplateTo(w, req, "Home", map[string]any{"title": "x"}); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestRenderTemplateRejectsNonObject(t *testing.T) {
	e, err := New(Config{Mode: ModeSSR, Renderer: &captureRenderer{html: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if _, err := e.RenderTemplate("Home", []int{1, 2}); err == nil {
		t.Fatal("非对象 data 应该返回 error")
	}
}

func TestNormalizeBase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"/", ""}, {"site", "/site"}, {"/site/", "/site"}, {"//a//", "//a"},
	} {
		if got := NormalizeBase(tc.in); got != tc.want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBasePathServesUnderPrefix(t *testing.T) {
	e := staticEngine(t, func(c *Config) { c.BasePath = "/site" })

	code, body, _ := serveReq(e, http.MethodGet, "/site/about", nil).result()
	if code != http.StatusOK || !strings.Contains(body, "prerendered-about") {
		t.Errorf("GET /site/about = %d %q", code, body)
	}

	// 资产走 StaticHandler 桥接（Gin 里挂 /site/assets/*filepath），同样要剥前缀
	code, _, _ = serveReq(e.StaticHandler(), http.MethodGet, "/site/assets/client.js", nil).result()
	if code != http.StatusOK {
		t.Errorf("GET /site/assets/client.js = %d, want 200（StaticHandler 也要剥前缀）", code)
	}

	// 前缀之外：引擎必须拒绝接管（交给上层路由去 404）
	code, _, _ = serveReq(e, http.MethodGet, "/about", nil).result()
	if code != http.StatusNotFound {
		t.Errorf("GET /about（无前缀）= %d, want 404", code)
	}
}

func TestClientScriptBasePrefix(t *testing.T) {
	e := staticEngine(t, func(c *Config) {
		c.BasePath = "/site"
		c.Renderer = &fakeRenderer{html: "x", head: ""}
	})
	if got := e.clientScript(); got != "/site/assets/client.js" {
		t.Errorf("clientScript() = %q, want /site/assets/client.js", got)
	}

	// 显式配置已带前缀 → 不重复补
	e2 := staticEngine(t, func(c *Config) {
		c.BasePath = "/site"
		c.ClientScript = "/site/assets/custom.js"
		c.Renderer = &fakeRenderer{html: "x", head: ""}
	})
	if got := e2.clientScript(); got != "/site/assets/custom.js" {
		t.Errorf("已带前缀的 ClientScript 被重复处理: %q", got)
	}

	// "-" 关闭注入
	e3 := staticEngine(t, func(c *Config) { c.ClientScript = "-" })
	if got := e3.clientScript(); got != "" {
		t.Errorf("ClientScript=%q 没有关闭注入", got)
	}

	// 产物里没有 assets/client.js（如只嵌 ssr.js 的 template 工程）→ 不注入
	ssrOnly := fstest.MapFS{"dist/ssr.js": {Data: []byte("x")}}
	e4, err := New(Config{EmbedFS: ssrOnly, DistDir: "dist", Renderer: &fakeRenderer{html: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer e4.Close()
	if got := e4.clientScript(); got != "" {
		t.Errorf("缺少 client.js 时不应注入，got %q", got)
	}
}

// ---------------------------------------------------------------------------
// assemble 契约：文档骨架、hydration 数据、XSS 转义
// ---------------------------------------------------------------------------

func TestAssembleEscapesPropsJSON(t *testing.T) {
	e := staticEngine(t, func(c *Config) {
		c.Renderer = &fakeRenderer{html: "x", head: ""}
		c.ClientScript = "-" // 只看数据本身
	})
	doc, err := e.RenderHTML("/x", Page{Props: map[string]any{"evil": "</script><script>alert(1)</script>"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(doc, "</script><script>alert(1)") {
		t.Errorf("props 未转义，存在 </script> 注入: %s", doc)
	}
	if !strings.Contains(doc, `\u003c/script\u003e`) {
		t.Errorf("应转义为 \\u003c 序列: %s", doc)
	}
	if !strings.Contains(doc, "<!doctype html>") || !strings.Contains(doc, `id="__WF_DATA__"`) {
		t.Errorf("文档骨架不完整: %s", doc)
	}
}

func TestAssemblePageHeadAndCSSInjection(t *testing.T) {
	// 1. 测试 Page.Head.Title 与 Description 自动注入
	e := staticEngine(t, func(c *Config) {
		c.Renderer = &fakeRenderer{html: "<div>content</div>", head: ""}
	})
	doc, err := e.RenderHTML("/seo", Page{
		Head: Head{
			Title:       "SEO 首页",
			Description: "这是一个测试描述",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "<title>SEO 首页</title>") {
		t.Fatalf("expected title injected in head, got: %s", doc)
	}
	if !strings.Contains(doc, `<meta name="description" content="这是一个测试描述">`) {
		t.Fatalf("expected description meta injected in head, got: %s", doc)
	}

	// 2. 若前端已经提供了 <title>，不应重复覆盖
	e2 := staticEngine(t, func(c *Config) {
		c.Renderer = &fakeRenderer{html: "<div>content</div>", head: "<title>前端自带标题</title>"}
	})
	doc2, err := e2.RenderHTML("/seo-dup", Page{
		Head: Head{Title: "不该覆盖的标题"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc2, "<title>前端自带标题</title>") {
		t.Fatalf("expected existing title to remain, got: %s", doc2)
	}
	if strings.Contains(doc2, "不该覆盖的标题") {
		t.Fatalf("should not override existing title, got: %s", doc2)
	}

	// 3. 测试 clientFS 中存在 assets/client.css 时的自动注入
	cssFS := fstest.MapFS{
		"dist/assets/client.js":  {Data: []byte("console.log('js')")},
		"dist/assets/client.css": {Data: []byte("body { color: red; }")},
	}
	eCSS, err := New(Config{
		Mode:     ModeSSR,
		EmbedFS:  cssFS,
		DistDir:  "dist",
		Renderer: &fakeRenderer{html: "<p>css</p>", head: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eCSS.Close()

	docCSS, err := eCSS.RenderHTML("/", Page{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(docCSS, `<link rel="stylesheet" href="/assets/client.css">`) {
		t.Fatalf("expected client.css link injected into head, got: %s", docCSS)
	}
}

// 静态模式没有 EmbedFS 也没有 Renderer 时必须显式报错，而不是半初始化。
func TestNewRequiresSourceOfPages(t *testing.T) {
	if _, err := New(Config{Mode: ModeSSR}); err == nil {
		t.Fatal("既无 Renderer 又无 EmbedFS 应当报错")
	}
}

// HEAD 请求：有长度、无 body、状态 200。
func TestHeadOnAsset(t *testing.T) {
	e := staticEngine(t)
	w := newPreset404Writer()
	req := httptest.NewRequest(http.MethodHead, "/assets/client.js", nil)
	e.StaticHandler().ServeHTTP(w, req)
	code, body, hdr := w.result()
	if code != http.StatusOK {
		t.Errorf("HEAD = %d", code)
	}
	if body != "" {
		t.Errorf("HEAD 带了 body")
	}
	if hdr.Get("Content-Length") == "" {
		t.Error("HEAD 缺 Content-Length")
	}
}

// failRenderer 模拟 goja 抛错：SSR 期间 render() 里 throw。
type failRenderer struct{ err error }

func (f *failRenderer) RenderHTML(url string, propsJSON []byte) (string, string, error) {
	return "", "", f.err
}
func (f *failRenderer) Close() error { return nil }

// SSR 渲染失败时绝不能静默：调用方普遍写 `_ = eng.Render(...)`，
// 若引擎只 return err，浏览器会收到空白的 200。
// 开发态要给一张带 goja 栈的错误页（Vite 的 error overlay 同款体验）。
func TestRenderErrorPageInDev(t *testing.T) {
	t.Setenv("WF_DEV", "")
	e, err := New(Config{
		Mode:     ModeSSR,
		Dev:      true,
		Renderer: &failRenderer{err: errors.New("render: Error: boom\n    at App (ssr.js:12:34)")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := newPreset404Writer()
	req := httptest.NewRequest(http.MethodGet, "/about", nil)
	if err := e.Render(w, req, Page{URL: "/about"}); err == nil {
		t.Fatal("渲染失败应同时返回 error 给调用方")
	}
	code, body, hdr := w.result()
	if code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q，期望 text/html（要能在浏览器里直接看）", ct)
	}
	for _, want := range []string{"SSR 渲染失败", "/about", "Error: boom", "at App (ssr.js:12:34)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("错误页缺少 %q:/n%s", want, body)
		}
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("错误页不该被缓存，Cache-Control = %q", hdr.Get("Cache-Control"))
	}
}

// 生产态不能把内部栈泄露给终端用户，但状态码必须是 500 而不是 200。
func TestRenderErrorHiddenInProd(t *testing.T) {
	t.Setenv("WF_DEV", "")
	e, err := New(Config{
		Mode:     ModeSSR,
		Renderer: &failRenderer{err: errors.New("render: secret internal detail")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := newPreset404Writer()
	_ = e.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), Page{URL: "/"})
	code, body, _ := w.result()
	if code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", code)
	}
	if strings.Contains(body, "secret internal detail") {
		t.Fatalf("生产态泄露了内部错误: %s", body)
	}
}

// ---------------------------------------------------------------------------
// 零配置模式解析：Renderer / pages/ / index.html 自动识别 SSR 与 SPA
// ---------------------------------------------------------------------------

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func indexOnlyFS() fstest.MapFS {
	return fstest.MapFS{
		"dist/index.html":       {Data: []byte("<!doctype html><html><body><div id=\"root\"></div><script type=\"module\" src=\"/assets/client.js\"></script></body></html>")},
		"dist/assets/client.js": {Data: []byte("console.log('client')")},
	}
}

// 有 Renderer 就是动态 SSR —— 哪怕 dist 里同时存在预渲染页。
func TestAutoModeWithRendererIsSSR(t *testing.T) {
	e := newTestEngine(t, Config{
		EmbedFS:  distMapFS(),
		DistDir:  "dist",
		Renderer: &fakeRenderer{html: `<div id="root">live-ssr</div>`},
	})
	if e.Mode() != ModeSSR {
		t.Fatalf("Mode() = %q, want ssr", e.Mode())
	}
	if e.Static() {
		t.Fatal("有 Renderer 时 Static() 应为 false")
	}
	code, body, _ := serveReq(e, http.MethodGet, "/about", nil).result()
	if code != http.StatusOK || !strings.Contains(body, "live-ssr") {
		t.Fatalf("自动 SSR 应动态渲染，got %d %q", code, body)
	}
	if strings.Contains(body, "prerendered-about") {
		t.Fatal("有 Renderer 时不应回落到预渲染页")
	}
}

// 没有 Renderer、但有 pages/**.html → 静态 / SSG。
func TestAutoModePrefersPrerenderedPages(t *testing.T) {
	e := newTestEngine(t, Config{EmbedFS: distMapFS(), DistDir: "dist"})
	if e.Mode() != ModeSSR || !e.Static() {
		t.Fatalf("Mode()=%q Static()=%v, want ssr/true", e.Mode(), e.Static())
	}
	code, body, _ := serveReq(e, http.MethodGet, "/about", nil).result()
	if code != http.StatusOK || !strings.Contains(body, "prerendered-about") {
		t.Fatalf("GET /about = %d %q", code, body)
	}
}

// pages 下没有 index.html 也算预渲染（routes.json 未必包含 "/"）。
func TestAutoModePagesWithoutIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"dist/pages/about.html": {Data: []byte("<!doctype html><html><body>only-about</body></html>")},
		"dist/index.html":       {Data: []byte("<!doctype html><html><body>spa-shell</body></html>")},
	}
	e := newTestEngine(t, Config{EmbedFS: fsys, DistDir: "dist"})
	if e.Mode() != ModeSSR || !e.Static() {
		t.Fatalf("有 pages/about.html 时应判为静态，got Mode()=%q", e.Mode())
	}
}

// 只有 index.html、没有 pages/ → SPA。
func TestAutoModeIndexOnlyIsSPA(t *testing.T) {
	e := newTestEngine(t, Config{EmbedFS: indexOnlyFS(), DistDir: "dist"})
	if e.Mode() != ModeSPA {
		t.Fatalf("Mode() = %q, want spa", e.Mode())
	}
	code, body, _ := serveReq(e, http.MethodGet, "/users/42", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("SPA fallback = %d %q", code, body)
	}
	code, _, _ = serveReq(e, http.MethodGet, "/assets/client.js", nil).result()
	if code != http.StatusOK {
		t.Fatalf("SPA 静态资源 = %d, want 200", code)
	}
}

// 空 dist（还没构建）不提前报错，保持旧的请求期 404。
func TestAutoModeEmptyDistFallsBackToStatic(t *testing.T) {
	fsys := fstest.MapFS{"dist/assets/client.js": {Data: []byte("x")}}
	e := newTestEngine(t, Config{EmbedFS: fsys, DistDir: "dist"})
	if e.Mode() != ModeSSR || !e.Static() {
		t.Fatalf("空 dist 应兜底为静态，got Mode()=%q Static()=%v", e.Mode(), e.Static())
	}
	code, _, _ := serveReq(e, http.MethodGet, "/", nil).result()
	if code != http.StatusNotFound {
		t.Fatalf("空 dist 请求 = %d, want 404", code)
	}
}

// 显式 Mode 永远覆盖自动探测。
func TestExplicitModeOverridesDetection(t *testing.T) {
	// 有 pages/ 也强制 SPA
	e := newTestEngine(t, Config{EmbedFS: distMapFS(), DistDir: "dist", Mode: ModeSPA})
	if e.Mode() != ModeSPA {
		t.Fatalf("Mode() = %q, want spa", e.Mode())
	}
	code, body, _ := serveReq(e, http.MethodGet, "/about", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("显式 SPA 应回外壳，got %d %q", code, body)
	}

	// 只有 index.html 也强制静态
	e2 := newTestEngine(t, Config{EmbedFS: indexOnlyFS(), DistDir: "dist", Mode: ModeSSR})
	if e2.Mode() != ModeSSR || !e2.Static() {
		t.Fatalf("Mode()=%q Static()=%v, want ssr/true", e2.Mode(), e2.Static())
	}
	code, _, _ = serveReq(e2, http.MethodGet, "/users/42", nil).result()
	if code != http.StatusNotFound {
		t.Fatalf("显式静态不应有 SPA 回退，got %d", code)
	}
}

// SPAHandler 与 Mode 无关：同一个 Engine 可以 SSR 为主，子路径交给 SPA。
func TestSPAHandler(t *testing.T) {
	e := newTestEngine(t, Config{EmbedFS: indexOnlyFS(), DistDir: "dist"})
	h := e.SPAHandler()

	code, body, _ := serveReq(h, http.MethodGet, "/app/users", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("SPAHandler fallback = %d %q", code, body)
	}
	code, _, _ = serveReq(h, http.MethodGet, "/assets/client.js", nil).result()
	if code != http.StatusOK {
		t.Fatalf("SPAHandler 静态资源 = %d, want 200", code)
	}

	// 没有 EmbedFS / index.html 时明确 404
	e2 := newTestEngine(t, Config{Renderer: &fakeRenderer{html: "x"}})
	code, body, _ = serveReq(e2.SPAHandler(), http.MethodGet, "/x", nil).result()
	if code != http.StatusNotFound || !strings.Contains(body, "SPA") {
		t.Fatalf("无 EmbedFS 的 SPAHandler = %d %q", code, body)
	}
}

func TestSPAHandlerRespectsBasePath(t *testing.T) {
	e := newTestEngine(t, Config{EmbedFS: indexOnlyFS(), DistDir: "dist", BasePath: "/site"})
	code, body, _ := serveReq(e.SPAHandler(), http.MethodGet, "/site/app/users", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("带前缀的 SPAHandler = %d %q", code, body)
	}
	code, _, _ = serveReq(e.SPAHandler(), http.MethodGet, "/app/users", nil).result()
	if code != http.StatusNotFound {
		t.Fatalf("前缀之外的请求 = %d, want 404", code)
	}
}

// 同一 engine + 同一份 EmbedFS：默认 SSR，/app/ 子路径走 SPA。
func TestOneEngineSSRAndSPA(t *testing.T) {
	e := newTestEngine(t, Config{
		EmbedFS:  distMapFS(),
		DistDir:  "dist",
		Renderer: &fakeRenderer{html: `<div id="root">live-ssr</div>`},
	})
	mux := http.NewServeMux()
	mux.Handle("/app/", e.SPAHandler())
	mux.Handle("/", e)

	code, body, _ := serveReq(mux, http.MethodGet, "/app/users", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("GET /app/users = %d %q", code, body)
	}
	if strings.Contains(body, "live-ssr") {
		t.Fatal("/app/ 不应走 SSR")
	}

	code, body, _ = serveReq(mux, http.MethodGet, "/about", nil).result()
	if code != http.StatusOK || !strings.Contains(body, "live-ssr") {
		t.Fatalf("GET /about = %d %q", code, body)
	}
}

// ModeSPA 只影响 ServeHTTP 的文档分派；显式调用 Render/RenderTemplate 仍可用 Renderer。
func TestModeSPAWithRendererKeepsRenderAPI(t *testing.T) {
	e := newTestEngine(t, Config{
		EmbedFS:  indexOnlyFS(),
		DistDir:  "dist",
		Mode:     ModeSPA,
		Renderer: &fakeRenderer{html: `<p>template</p>`},
	})
	code, body, _ := serveReq(e, http.MethodGet, "/x", nil).result()
	if code != http.StatusOK || !strings.Contains(body, `<div id="root"></div>`) {
		t.Fatalf("ServeHTTP = %d %q", code, body)
	}
	w := httptest.NewRecorder()
	if err := e.RenderTemplateTo(w, httptest.NewRequest(http.MethodGet, "/", nil), "T", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), "<p>template</p>") {
		t.Fatalf("RenderTemplateTo 没用 Renderer: %q", w.Body.String())
	}
}

// 模板用法只需 Renderer：没有 EmbedFS 时自动 SSR，也不会注入客户端脚本。
func TestTemplateEngineMinimalConfig(t *testing.T) {
	e := newTestEngine(t, Config{Renderer: &captureRenderer{html: `<p>t</p>`}})
	if e.Mode() != ModeSSR || e.Static() {
		t.Fatalf("Mode()=%q Static()=%v, want ssr/false", e.Mode(), e.Static())
	}
	if src := e.clientScript(); src != "" {
		t.Fatalf("无 EmbedFS 时不应注入客户端脚本，got %q", src)
	}
}

// HTML shell 扩展：调用方可以补 html/body 属性与 head 标签，而不必替换模板。
func TestRenderHTMLShellExtensions(t *testing.T) {
	e := newTestEngine(t, Config{Renderer: &fakeRenderer{html: `<div id="root">x</div>`, head: `<style>.x{}</style>`}})
	doc, err := e.RenderHTML("/", Page{
		Props:     map[string]any{"locale": "en"},
		Head:      Head{Title: "Shell"},
		HTMLAttrs: `lang="en"`,
		BodyAttrs: `class="plaza-body"`,
		HeadHTML:  `<meta name="viewport" content="width=device-width, initial-scale=1.0">`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<html lang="en">`,
		`<body class="plaza-body">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1.0">`,
		`<title>Shell</title>`,
		`__WF_DATA__`,
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("shell 缺少 %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, `<html >`) || strings.Contains(doc, `<body >`) {
		t.Fatalf("空属性不应留下多余空格:\n%s", doc)
	}
}

// SetRenderer：旧渲染器的在途渲染完成后才关闭，新请求立即使用新渲染器。
func TestSetRendererWaitsForInflight(t *testing.T) {
	old := &swapRenderer{html: "old", started: make(chan struct{}), release: make(chan struct{})}
	e, err := New(Config{Mode: ModeSSR, Renderer: old})
	if err != nil {
		t.Fatal(err)
	}

	renderDone := make(chan error, 1)
	go func() {
		_, err := e.RenderHTML("/", Page{})
		renderDone <- err
	}()
	select {
	case <-old.started:
	case <-time.After(2 * time.Second):
		t.Fatal("旧渲染器没有被调用")
	}

	next := &swapRenderer{html: "new"}
	swapDone := make(chan error, 1)
	go func() { swapDone <- e.SetRenderer(next) }()

	select {
	case err := <-swapDone:
		t.Fatalf("SetRenderer 不应在旧渲染完成前返回: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if old.closed.Load() {
		t.Fatal("旧渲染器在在途渲染结束前被关闭")
	}
	close(old.release)
	if err := <-renderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-swapDone; err != nil {
		t.Fatal(err)
	}
	if !old.closed.Load() {
		t.Fatal("旧渲染器没有关闭")
	}

	doc, err := e.RenderHTML("/", Page{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "new") || strings.Contains(doc, "old") {
		t.Fatalf("热换后应使用新渲染器:\n%s", doc)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if !next.closed.Load() {
		t.Fatal("Close 应关闭当前渲染器")
	}
}

type swapRenderer struct {
	html    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (r *swapRenderer) RenderHTML(url string, propsJSON []byte) (string, string, error) {
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	if r.release != nil {
		<-r.release
	}
	return r.html, "", nil
}

func (r *swapRenderer) Close() error {
	r.closed.Store(true)
	return nil
}

// WF_DEV 由 wf dev 注入：不改代码也能让错误可见。
func TestRenderErrorVisibleViaEnv(t *testing.T) {
	t.Setenv("WF_DEV", "1")
	e, err := New(Config{
		Mode:     ModeSSR,
		Renderer: &failRenderer{err: errors.New("render: env-visible")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := newPreset404Writer()
	_ = e.Render(w, httptest.NewRequest(http.MethodGet, "/", nil), Page{URL: "/"})
	_, body, _ := w.result()
	if !strings.Contains(body, "env-visible") {
		t.Fatalf("WF_DEV=1 时应输出错误页，实际:\n%s", body)
	}
}
