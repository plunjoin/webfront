package webfront

// webfront_test.go：核心运行时的库级测试，全部只依赖标准库。
//
// 关键场景是"Gin NoRoute 兜底"：Gin 在调用兜底处理器**之前**就把响应状态预设成
// 404（gin.serveError 里 c.writermem.status = 404），首个 Write 会把这个预设值
// 落盘。这里用 preset404Writer 复刻该语义（不 import Gin，保持库零框架依赖），
// 钉死"渲染成功 → 状态码必须 200"这条契约。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
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
