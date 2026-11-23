//go:build dev

package dev

import (
	"encoding/json"
	htmlpkg "html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSSRRenderer 记录调用参数并返回固定片段；Close 用原子标记断言换池时
// 旧渲染器确实被关闭。
type fakeSSRRenderer struct {
	html, head string
	err        error
	closed     atomic.Bool
	mu         sync.Mutex
	calls      int
	lastURL    string
	lastProps  string
}

func (f *fakeSSRRenderer) RenderHTML(url string, props []byte) (string, string, error) {
	f.mu.Lock()
	f.calls++
	f.lastURL = url
	f.lastProps = string(props)
	f.mu.Unlock()
	return f.html, f.head, f.err
}

func (f *fakeSSRRenderer) Close() error {
	f.closed.Store(true)
	return nil
}

// blockingRenderer 卡住一次渲染，用来验证换池会等在途请求结束。
type blockingRenderer struct {
	started chan struct{}
	release chan struct{}
	closed  atomic.Bool
}

func (b *blockingRenderer) RenderHTML(string, []byte) (string, string, error) {
	close(b.started)
	<-b.release
	return `<div>slow SSR</div>`, "", nil
}

func (b *blockingRenderer) Close() error {
	b.closed.Store(true)
	return nil
}

func TestDevSSRSwapWaitsForInflightRender(t *testing.T) {
	ds := &DevServer{
		ssrEntry: "src/entry-server.ts",
		clients:  map[*wsClient]struct{}{},
	}
	old := &blockingRenderer{started: make(chan struct{}), release: make(chan struct{})}
	next := &fakeSSRRenderer{html: `<div>new</div>`}
	ds.ssrRenderer = old

	rendered := make(chan error, 1)
	go func() {
		_, err := ds.renderDocument(httptest.NewRequest(http.MethodGet, "/gallery", nil), Page{SSR: true})
		rendered <- err
	}()
	<-old.started

	swapped := make(chan struct{})
	go func() {
		ds.swapSSR(next)
		close(swapped)
	}()
	select {
	case <-swapped:
		t.Fatal("在途渲染未结束时 swapSSR 不应返回")
	case <-time.After(50 * time.Millisecond):
	}
	if old.closed.Load() {
		t.Fatal("在途渲染未结束时旧 renderer 不应被关闭")
	}

	close(old.release)
	if err := <-rendered; err != nil {
		t.Fatalf("在途渲染失败: %v", err)
	}
	<-swapped
	if !old.closed.Load() {
		t.Fatal("swap 完成后旧 renderer 应被关闭")
	}
	if next.closed.Load() {
		t.Fatal("新 renderer 不应被关闭")
	}
}

func writeShellFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRenderPageDefaultShell(t *testing.T) {
	tmp := t.TempDir()
	writeShellFile(t, tmp, "src/main.ts", `export function render() {}`)
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/studio/works?tab=all", nil), Page{
		Title:       "Studio <Works>",
		Description: `A "quoted" description`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("RenderPage = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		"<title>Studio &lt;Works&gt;</title>",
		`<meta name="description" content="A &#34;quoted&#34; description">`,
		`<div id="root"></div>`,
		`<script src="/@wf/overlay.js"></script>`,
		`<script type="module" src="/@wf/boot"></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("默认外壳缺少 %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"__WF_DATA__", "__WF_BOOTSTRAP__", wfHeadMarker, wfBodyMarker, wfScriptsMarker} {
		if strings.Contains(body, unwanted) {
			t.Errorf("默认外壳不应包含 %q:\n%s", unwanted, body)
		}
	}
}

func TestRenderPageRequiresSSROption(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/gallery", nil), Page{SSR: true})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Page.SSR 无 WithSSR 应 500，got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "WithSSR") {
		t.Fatalf("错误信息应指出缺 WithSSR: %s", rec.Body.String())
	}
}

func TestRenderPageSSRWithShellFunc(t *testing.T) {
	tmp := t.TempDir()
	writeShellFile(t, tmp, "src/entry-server.ts", `export function render() { return { html: "", head: "" } }`)
	renderer := &fakeSSRRenderer{
		html: `<div id="app">SSR body</div>`,
		head: `<style data-ssr>.hero{color:red}</style>`,
	}
	var factoryCalls int32
	ds, err := NewDevServer(tmp,
		WithSSR("src/entry-server.ts", func(bundle []byte) (SSRRenderer, error) {
			atomic.AddInt32(&factoryCalls, 1)
			return renderer, nil
		}),
		WithHTMLShell(func(data ShellData) ([]byte, error) {
			if data.Request == nil {
				t.Error("ShellData.Request 为空")
			}
			return []byte(`<!doctype html><html><head><title>` + htmlpkg.EscapeString(data.Title) +
				`</title>` + wfHeadMarker + `</head><body data-url="` + data.URL + `">` +
				`<main id="root">` + wfBodyMarker + `</main>` + wfScriptsMarker + `</body></html>`), nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if n := atomic.LoadInt32(&factoryCalls); n != 1 {
		t.Fatalf("启动时应同步编译一次 SSR，factory calls = %d", n)
	}

	props := map[string]any{"locale": "en"}
	bootstrap := map[string]any{"githubLogin": true}
	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/gallery", nil), Page{
		SSR:       true,
		Title:     "Gallery",
		Props:     props,
		Bootstrap: bootstrap,
		HeadHTML:  `<meta name="robots" content="noindex">`,
		HTMLAttrs: `lang="en"`,
		BodyAttrs: `class="plaza"`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("RenderPage = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<title>Gallery</title>`,
		`<style data-ssr>.hero{color:red}</style>`,
		`<main id="root"><div id="app">SSR body</div></main>`,
		`<script id="__WF_DATA__" type="application/json">{"locale":"en"}</script>`,
		`<script id="__WF_BOOTSTRAP__" type="application/json">{"githubLogin":true}</script>`,
		`<div id="app">SSR body</div>`,
		`data-url="/gallery"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSR 外壳缺少 %q:\n%s", want, body)
		}
	}
	// ShellFunc 自己决定骨架：HeadHTML/HTMLAttrs/BodyAttrs 不应被服务端二次注入。
	for _, unwanted := range []string{wfHeadMarker, wfBodyMarker, wfScriptsMarker, `<meta name="robots"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("ShellFunc 页面不应包含 %q:\n%s", unwanted, body)
		}
	}
	renderer.mu.Lock()
	lastURL, lastProps := renderer.lastURL, renderer.lastProps
	renderer.mu.Unlock()
	if lastURL != "/gallery" {
		t.Errorf("SSR URL = %q", lastURL)
	}
	if lastProps != `{"locale":"en"}` {
		t.Errorf("SSR props = %s", lastProps)
	}
}

func TestRenderPageShellFile(t *testing.T) {
	tmp := t.TempDir()
	writeShellFile(t, tmp, "shell.html", `<!doctype html><html><head><title>old</title>`+
		`<meta name="description" content="old description">`+
		`<link rel="stylesheet" href="/assets/client.css"></head>`+
		`<body class="old"><div id="root"></div>`+
		`<script type="module" src="/assets/client.js"></script></body></html>`)

	ds, err := NewDevServer(tmp, WithHTMLShellFile("shell.html"))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/download", nil), Page{
		Title:       "Download",
		Description: "Get the app",
		HTMLAttrs:   `lang="zh-CN"`,
		BodyAttrs:   `data-page="download"`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("RenderPage = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<html lang="zh-CN">`,
		`<title>Download</title>`,
		`<meta name="description" content="Get the app">`,
		`<body class="old" data-page="download">`,
		`<div id="root"></div>`,
		`<script src="/@wf/overlay.js"></script>`,
		`<script type="module" src="/@wf/boot"></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell 文件页面缺少 %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"/assets/client.js", "/assets/client.css", "<title>old</title>"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("shell 文件页面不应包含 %q:\n%s", unwanted, body)
		}
	}
}

func TestRenderPageSPABootstrap(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/login", nil), Page{
		Props:     map[string]any{"should": "not appear"},
		Bootstrap: map[string]any{"locale": "zh-CN"},
	})
	body := rec.Body.String()
	if !strings.Contains(body, `id="__WF_BOOTSTRAP__"`) || !strings.Contains(body, `{"locale":"zh-CN"}`) {
		t.Fatalf("SPA 页面应注入 __WF_BOOTSTRAP__: %s", body)
	}
	if strings.Contains(body, "__WF_DATA__") || strings.Contains(body, "should") {
		t.Fatalf("SPA 页面不应注入 __WF_DATA__（否则会 hydrate 空 root）: %s", body)
	}
}

func TestDevSSRRebuildSwapsAndRecovers(t *testing.T) {
	tmp := t.TempDir()
	writeShellFile(t, tmp, "src/entry-server.ts", `export function render() { return { html: "", head: "" } }`)

	var mu sync.Mutex
	var renderers []*fakeSSRRenderer
	factory := func(bundle []byte) (SSRRenderer, error) {
		r := &fakeSSRRenderer{html: `<div>ssr</div>`}
		mu.Lock()
		renderers = append(renderers, r)
		mu.Unlock()
		return r, nil
	}
	ds := &DevServer{
		root:       tmp,
		modulesDir: "node_modules",
		ssrEntry:   "src/entry-server.ts",
		ssrFactory: factory,
		ssrTrigger: make(chan struct{}, 1),
		closed:     make(chan struct{}),
		clients:    map[*wsClient]struct{}{},
	}
	defer ds.Close()

	ds.rebuildSSR(false)
	if st := ds.SSRStatus(); !st.RendererReady || st.Builds != 1 || st.Failed {
		t.Fatalf("第一次构建状态 = %+v", st)
	}
	ds.rebuildSSR(false)
	st := ds.SSRStatus()
	if !st.RendererReady || st.Builds != 2 || st.Failed {
		t.Fatalf("第二次构建状态 = %+v", st)
	}
	mu.Lock()
	if len(renderers) != 2 {
		mu.Unlock()
		t.Fatalf("factory calls = %d", len(renderers))
	}
	first, second := renderers[0], renderers[1]
	mu.Unlock()
	if !first.closed.Load() {
		t.Fatal("换池后旧 renderer 未被关闭")
	}
	if second.closed.Load() {
		t.Fatal("新 renderer 不应被关闭")
	}

	// 编译失败：保留旧 renderer，状态记下错误。
	writeShellFile(t, tmp, "src/entry-server.ts", `export const = ;`)
	ds.rebuildSSR(false)
	st = ds.SSRStatus()
	if !st.RendererReady || st.Builds != 2 || !st.Failed || !strings.Contains(st.LastError, "entry-server.ts") {
		t.Fatalf("失败后的状态 = %+v", st)
	}
	if second.closed.Load() {
		t.Fatal("构建失败不应关闭仍在使用的 renderer")
	}

	// 修好：清除失败标记并再次换池。
	writeShellFile(t, tmp, "src/entry-server.ts", `export function render() { return { html: "", head: "" } }`)
	ds.rebuildSSR(false)
	st = ds.SSRStatus()
	if st.Builds != 3 || st.Failed || st.LastError != "" || !st.RendererReady {
		t.Fatalf("恢复后的状态 = %+v", st)
	}
	if !second.closed.Load() {
		t.Fatal("恢复换池后上一代 renderer 应被关闭")
	}
}

func TestWithSSRConfigValidation(t *testing.T) {
	tmp := t.TempDir()
	if _, err := NewDevServer(tmp, WithSSR("src/entry-server.ts", nil)); err == nil {
		t.Fatal("WithSSR(entry, nil) 应报错")
	}
	if _, err := NewDevServer(tmp, WithSSR("", func([]byte) (SSRRenderer, error) { return nil, nil })); err == nil {
		t.Fatal("WithSSR(\"\", factory) 应报错")
	}
	if _, err := NewDevServer(tmp, WithHTMLShellFile("missing-shell.html")); err == nil {
		t.Fatal("缺失的 shell 文件应报错")
	}
}

func TestImportMapDataEscapesScriptClose(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	// </script> 必须被 json.Marshal 转义，不能提前闭合数据标签。
	rec := httptest.NewRecorder()
	ds.RenderPage(rec, httptest.NewRequest(http.MethodGet, "/x", nil), Page{
		Bootstrap: map[string]any{"evil": "</script><script>alert(1)</script>"},
	})
	body := rec.Body.String()
	if strings.Contains(body, "</script><script>alert(1)") {
		t.Fatalf("__WF_BOOTSTRAP__ JSON 未转义 </script>: %s", body)
	}
	start := strings.Index(body, `id="__WF_BOOTSTRAP__"`)
	if start < 0 {
		t.Fatalf("缺少 __WF_BOOTSTRAP__: %s", body)
	}
	end := strings.Index(body[start:], "</script>")
	if end < 0 {
		t.Fatalf("数据标签未闭合: %s", body)
	}
	raw := body[start+len(`id="__WF_BOOTSTRAP__" type="application/json">`) : start+end]
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("数据标签不是合法 JSON: %v\n%s", err, raw)
	}
	if !strings.Contains(decoded["evil"].(string), "</script>") {
		t.Fatalf("转义后的数据应还原原始值: %+v", decoded)
	}
}
