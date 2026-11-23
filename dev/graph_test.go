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
	"time"

	"github.com/gorilla/websocket"

	"github.com/plunjoin/webfront/module"
)

func writeGraphFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findImport(imports []module.Import, spec string) (module.Import, bool) {
	for _, imp := range imports {
		if imp.Specifier == spec {
			return imp, true
		}
	}
	return module.Import{}, false
}

// 切片 0 的核心契约：每个被服务的模块都有 canonical id、version 和 deps，
// 并且 importers 能从反向索引里查到；静态资源 import 返回 URL 模块，
// 原始字节走 /@wf/asset/。
func TestDevGraphImportsImportersAndAssets(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/App.tsx", `
import Child from "./Child";
import logo from "./logo.svg";
export default function App() { return <div>{Child}{logo}</div> }
`)
	writeGraphFile(t, tmp, "src/Child.tsx", `export default "child";`)
	writeGraphFile(t, tmp, "src/logo.svg", `<svg xmlns="http://www.w3.org/2000/svg"><circle/></svg>`)

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := ds.Handler()

	rec := doGet(h, "/src/App.tsx")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /src/App.tsx = %d: %s", rec.Code, rec.Body.String())
	}
	code := rec.Body.String()
	if !strings.Contains(code, `"./Child"`) || !strings.Contains(code, `"./logo.svg"`) {
		t.Fatalf("dev 产物不应改写浏览器可解析的说明符:\n%s", code)
	}

	app, ok := ds.Module("src/App.tsx")
	if !ok {
		t.Fatalf("模块图里没有 src/App.tsx: %+v", ds.Graph().Nodes())
	}
	if app.ID != "src/App.tsx" || app.Kind != module.KindJS || app.Version == 0 || app.Hash == "" {
		t.Fatalf("App 节点字段不完整: %+v", app)
	}
	child, ok := findImport(app.Imports, "./Child")
	if !ok || child.Resolved != "src/Child.tsx" || child.External || child.Asset {
		t.Fatalf("./Child import = %+v, ok=%v", child, ok)
	}
	logo, ok := findImport(app.Imports, "./logo.svg")
	if !ok || logo.Resolved != "src/logo.svg" || !logo.Asset || logo.External {
		t.Fatalf("./logo.svg import = %+v, ok=%v", logo, ok)
	}

	// 同一模块重复请求：内容没变，version 不应增长。
	if rec := doGet(h, "/src/App.tsx"); rec.Code != http.StatusOK {
		t.Fatalf("second GET = %d", rec.Code)
	}
	app2, _ := ds.Module("src/App.tsx")
	if app2.Version != app.Version {
		t.Fatalf("重复请求不应提升 version: %d -> %d", app.Version, app2.Version)
	}

	// /src 与 /@fs 必须落到同一个节点，URL 统一成 canonical /src 形式。
	// /@fs/src/... 会被 302 到 /src/...，浏览器跟随后再编译入图。
	rec = doGet(h, "/@fs/src/App.tsx")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/src/App.tsx" {
		t.Fatalf("GET /@fs/src/App.tsx = %d %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if rec := doGet(h, "/src/App.tsx"); rec.Code != http.StatusOK {
		t.Fatalf("GET /src/App.tsx（canonical）= %d: %s", rec.Code, rec.Body.String())
	}
	appFS, _ := ds.Module("src/App.tsx")
	if appFS.URL != "/src/App.tsx" || appFS.Version != app.Version {
		t.Fatalf("/@fs 与 /src 不是同一节点: before=%+v after=%+v", app, appFS)
	}

	// 只有真正请求过 Child 后它才成为节点，但 importers 关系在 App 入图时就有。
	if rec := doGet(h, "/src/Child.tsx"); rec.Code != http.StatusOK {
		t.Fatalf("GET /src/Child.tsx = %d: %s", rec.Code, rec.Body.String())
	}
	importers := ds.Graph().Importers("src/Child.tsx")
	if len(importers) != 1 || importers[0] != "src/App.tsx" {
		t.Fatalf("Child importers = %v, want [src/App.tsx]", importers)
	}
	childNode, _ := ds.Module("src/Child.tsx")
	if childNode.URL != "/src/Child.tsx" {
		t.Fatalf("Child URL = %q", childNode.URL)
	}

	// import './logo.svg' 得到 URL 模块；真实字节走 asset 路由。
	rec = doGet(h, "/src/logo.svg")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /src/logo.svg = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `export default "/@wf/asset/src/logo.svg"`) {
		t.Fatalf("asset 模块没有默认导出 URL: %s", rec.Body.String())
	}
	asset, ok := ds.Module("src/logo.svg")
	if !ok || asset.Kind != module.KindAsset {
		t.Fatalf("asset 节点 = %+v, ok=%v", asset, ok)
	}

	rec = doGet(h, "/@wf/asset/src/logo.svg")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("GET /@wf/asset/src/logo.svg = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "svg") {
		t.Fatalf("asset Content-Type = %q", ct)
	}

	rec = doGet(h, "/src/logo.svg?raw")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "export default") ||
		!strings.Contains(rec.Body.String(), "svg") {
		t.Fatalf("?raw 没有返回文本模块: %d %s", rec.Code, rec.Body.String())
	}
}

// CSS 也走 Build+Metafile：@import 与 url() 都进图，但输出仍交给浏览器解析。
func TestDevGraphCSSImports(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/style.css", `@import "./base.css"; .a { background: url("./logo.svg"); }`)
	writeGraphFile(t, tmp, "src/base.css", `body { color: red; }`)
	writeGraphFile(t, tmp, "src/logo.svg", `<svg/>`)

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rec := doGet(ds.Handler(), "/src/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /src/style.css = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data-wf-id") || !strings.Contains(body, "document.createElement('style')") {
		t.Fatalf("CSS 模块缺少稳定 style id/注入逻辑: %s", body)
	}
	style, ok := ds.Module("src/style.css")
	if !ok || style.Kind != module.KindCSS {
		t.Fatalf("style 节点 = %+v, ok=%v", style, ok)
	}
	base, ok := findImport(style.Imports, "./base.css")
	if !ok || base.Resolved != "src/base.css" || base.Kind != "import-rule" || base.External {
		t.Fatalf("@import 边 = %+v, ok=%v", base, ok)
	}
	logo, ok := findImport(style.Imports, "./logo.svg")
	if !ok || logo.Resolved != "src/logo.svg" || !logo.Asset || logo.Kind != "url-token" {
		t.Fatalf("url() 边 = %+v, ok=%v", logo, ok)
	}

	if rec := doGet(ds.Handler(), "/src/base.css"); rec.Code != http.StatusOK {
		t.Fatalf("GET /src/base.css = %d: %s", rec.Code, rec.Body.String())
	}
	if got := ds.Graph().Importers("src/base.css"); len(got) != 1 || got[0] != "src/style.css" {
		t.Fatalf("base.css importers = %v", got)
	}
}

// Close 必须可重复调用，并真正断开 HMR 客户端。
func TestDevServerClose(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/main.tsx", `export const render = () => {}`)
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ds.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/__wf_hmr"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 先消费 connected 握手帧，确保缓冲里没有 Close 前已排队的消息。
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取 connected: %v", err)
	}
	var hello hmrConnected
	if err := json.Unmarshal(first, &hello); err != nil {
		t.Fatalf("connected 不是 JSON: %v: %s", err, first)
	}
	if hello.Type != "connected" {
		t.Fatalf("首帧不是 connected: %+v", hello)
	}

	if err := ds.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ds.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("Close 后 WebSocket 仍然可读")
	}
}

// 一次保存的多个 fsnotify 事件应合并成一个短窗口、一条 batch；
// 连接建立后的第一条消息必须是带能力表的 connected。
func TestDevServerBroadcastsUpdate(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/App.tsx", `export const render = () => {}`)
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	// 先让浏览器"加载过" App.tsx：没入图的模块发生变化时服务端不广播。
	if rec := doGet(ds.Handler(), "/src/App.tsx"); rec.Code != http.StatusOK {
		t.Fatalf("GET App.tsx = %d: %s", rec.Code, rec.Body.String())
	}
	srv := httptest.NewServer(ds.Handler())
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/__wf_hmr"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("没有收到 connected: %v", err)
	}
	var hello hmrConnected
	if err := json.Unmarshal(msg, &hello); err != nil {
		t.Fatalf("connected 不是 JSON: %v: %s", err, msg)
	}
	if hello.Type != "connected" || hello.Protocol != hmrProtocol {
		t.Fatalf("connected 协议字段不对: %+v", hello)
	}
	if hello.BuildID == "" || !hello.Capabilities["batch"] || !hello.Capabilities["vue"] {
		t.Fatalf("connected 缺少 buildId/能力表: %+v", hello)
	}

	if err := os.WriteFile(filepath.Join(tmp, "src", "App.tsx"),
		[]byte(`export const render = () => { return 2 }`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("没有收到 HMR batch: %v", err)
	}
	var batch hmrBatch
	if err := json.Unmarshal(msg, &batch); err != nil {
		t.Fatalf("batch 不是 JSON: %v: %s", err, msg)
	}
	// App.tsx 没有被谁 import，也没有 self-accept：唯一正确的行为是整页刷新。
	if batch.Type != "batch" || batch.Seq == 0 || len(batch.Updates) != 1 ||
		batch.Updates[0].Type != "full-reload" {
		t.Fatalf("batch 消息不对: %s", msg)
	}
}

// 删除文件时图节点必须移除，避免 imports/importers 指向不存在的模块。
func TestDevGraphRemoveOnDelete(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/Child.tsx", `export default 1`)
	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	if rec := doGet(ds.Handler(), "/src/Child.tsx"); rec.Code != http.StatusOK {
		t.Fatalf("GET Child = %d", rec.Code)
	}
	if _, ok := ds.Module("src/Child.tsx"); !ok {
		t.Fatal("Child 未入图")
	}
	if err := os.Remove(filepath.Join(tmp, "src", "Child.tsx")); err != nil {
		t.Fatal(err)
	}
	// fsnotify 在 Windows 上可能稍晚送达；最多等 2 秒。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ds.Module("src/Child.tsx"); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("删除文件后图节点仍然存在")
}
