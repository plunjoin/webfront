//go:build dev

package dev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/plunjoin/webfront/module"
)

// captureBatches 直接挂一个假客户端到广播表：batch 只写入 send channel，
// 不需要真的跑 WebSocket writer，测试因此是确定性的。
func captureBatches(t *testing.T, d *DevServer) chan []byte {
	t.Helper()
	ch := make(chan []byte, 16)
	c := &wsClient{send: ch, done: make(chan struct{})}
	d.mu.Lock()
	if d.clients == nil {
		d.clients = map[*wsClient]struct{}{}
	}
	d.clients[c] = struct{}{}
	d.mu.Unlock()
	t.Cleanup(func() {
		d.mu.Lock()
		delete(d.clients, c)
		d.mu.Unlock()
	})
	return ch
}

func nextBatch(t *testing.T, ch chan []byte) hmrBatch {
	t.Helper()
	select {
	case raw := <-ch:
		var b hmrBatch
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("batch 不是 JSON: %v: %s", err, raw)
		}
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到 batch")
		return hmrBatch{}
	}
}

func noBatch(t *testing.T, ch chan []byte) {
	t.Helper()
	select {
	case raw := <-ch:
		t.Fatalf("不应收到 batch: %s", raw)
	case <-time.After(150 * time.Millisecond):
	}
}

func currentSeq(d *DevServer) int64 {
	d.hmrMu.RLock()
	defer d.hmrMu.RUnlock()
	return d.seq
}

func newHMRServer(t *testing.T, files map[string]string, opts ...Option) (*DevServer, func(string) string) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		writeGraphFile(t, root, rel, body)
	}
	ds, err := NewDevServer(root, opts...)
	if err != nil {
		t.Fatalf("NewDevServer: %v", err)
	}
	t.Cleanup(func() { _ = ds.Close() })
	get := func(urlPath string) string {
		t.Helper()
		rec := doGet(ds.Handler(), urlPath)
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d: %s", urlPath, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	return ds, get
}

func imp(spec, resolved string) module.Import {
	return module.Import{Specifier: spec, Resolved: resolved, Kind: "import-statement"}
}

func addNode(ds *DevServer, id string, kind module.Kind, self bool, hmrKind string, imports ...module.Import) {
	ds.graph.Set(module.Node{
		ID:         id,
		URL:        "/" + id,
		Kind:       kind,
		SelfAccept: self,
		HMRKind:    hmrKind,
		Imports:    imports,
	})
}

// 边界冒泡的纯规划测试：不经过 esbuild/文件系统，直接构造图。
func TestHMRPlanBoundaries(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	addNode(ds, "src/main.tsx", module.KindJS, false, "", imp("./App.vue", "src/App.vue"))
	addNode(ds, "src/App.vue", module.KindVue, true, "vue", imp("./Child", "src/Child.ts"))
	addNode(ds, "src/Child.ts", module.KindJS, false, "", imp("./util", "src/util.ts"))
	addNode(ds, "src/util.ts", module.KindJS, false, "")
	addNode(ds, "src/orphan.ts", module.KindJS, false, "")

	// util 不是边界 → 冒泡到 App.vue；更新目标是 App.vue 自己。
	got, full := ds.planBoundaries("src/util.ts")
	if full != "" || strings.Join(got, ",") != "src/App.vue" {
		t.Fatalf("util boundaries = %v, full=%q", got, full)
	}
	// 自 accept 的 Vue 组件直接就是边界。
	got, full = ds.planBoundaries("src/App.vue")
	if full != "" || strings.Join(got, ",") != "src/App.vue" {
		t.Fatalf("App boundaries = %v, full=%q", got, full)
	}
	// 中间层变化同样冒泡到 App.vue。
	got, full = ds.planBoundaries("src/Child.ts")
	if full != "" || strings.Join(got, ",") != "src/App.vue" {
		t.Fatalf("Child boundaries = %v, full=%q", got, full)
	}
	// 入口变化、无 importers 的孤儿模块都必须整页刷新。
	if _, full = ds.planBoundaries("src/main.tsx"); full == "" {
		t.Fatal("入口变化应 full reload")
	}
	if _, full = ds.planBoundaries("src/orphan.ts"); full == "" {
		t.Fatal("无边界孤儿模块应 full reload")
	}

	// 环：a ↔ b，没有任何 self-accept。BFS 必须收敛并退化成 full reload。
	addNode(ds, "src/a.ts", module.KindJS, false, "", imp("./b", "src/b.ts"))
	addNode(ds, "src/b.ts", module.KindJS, false, "", imp("./a", "src/a.ts"))
	if _, full = ds.planBoundaries("src/a.ts"); full == "" {
		t.Fatal("无边界环应 full reload")
	}

	// 多个 importers，其中 X 自己 accept：返回去重后的所有边界。
	addNode(ds, "src/X.ts", module.KindJS, true, "", imp("./util", "src/util.ts"))
	addNode(ds, "src/Y.ts", module.KindJS, false, "", imp("./util", "src/util.ts"))
	addNode(ds, "src/App.vue", module.KindVue, true, "vue",
		imp("./Child", "src/Child.ts"), imp("./Y", "src/Y.ts"))
	got, full = ds.planBoundaries("src/util.ts")
	if full != "" || strings.Join(got, ",") != "src/App.vue,src/X.ts" {
		t.Fatalf("多 importer boundaries = %v, full=%q", got, full)
	}

	// CSS 是内置边界；用户手写 accept 的 JS 也是。
	addNode(ds, "src/theme.css", module.KindCSS, true, "css")
	addNode(ds, "src/App.vue", module.KindVue, true, "vue",
		imp("./Child", "src/Child.ts"), imp("./Y", "src/Y.ts"), imp("./theme.css", "src/theme.css"))
	got, full = ds.planBoundaries("src/theme.css")
	if full != "" || strings.Join(got, ",") != "src/theme.css" {
		t.Fatalf("css boundaries = %v, full=%q", got, full)
	}
	addNode(ds, "src/Child.ts", module.KindJS, true, "", imp("./util", "src/util.ts"))
	got, full = ds.planBoundaries("src/util.ts")
	if full != "" || !strings.Contains(strings.Join(got, ","), "src/Child.ts") {
		t.Fatalf("JS accept boundaries = %v, full=%q", got, full)
	}
}

// 一次防抖窗口内的多个文件 = 一条 batch、一个 seq；两个边界都更新。
func TestHMRBatchCoalesces(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import "./A.js"; import "./B.js"; export function render() {}`,
		"src/A.js":    `import { a } from "./a.ts"; import.meta.hot.accept(function () {}); export const A = a;`,
		"src/B.js":    `import { b } from "./b.ts"; import.meta.hot.accept(function () {}); export const B = b;`,
		"src/a.ts":    `export const a = 1;`,
		"src/b.ts":    `export const b = 2;`,
	})
	for _, p := range []string{"/src/main.ts", "/src/A.js", "/src/B.js", "/src/a.ts", "/src/b.ts"} {
		get(p)
	}
	ch := captureBatches(t, ds)
	ds.queueChange("src/a.ts", "/src/a.ts")
	ds.queueChange("src/b.ts", "/src/b.ts")
	ds.flushChanges()

	batch := nextBatch(t, ch)
	if batch.Type != "batch" || batch.Seq != 1 {
		t.Fatalf("batch = %+v", batch)
	}
	if len(batch.Updates) != 2 ||
		batch.Updates[0].Path != "/src/A.js" || batch.Updates[1].Path != "/src/B.js" {
		t.Fatalf("updates = %+v", batch.Updates)
	}
	for _, u := range batch.Updates {
		if u.Type != "update" || u.Kind != "js" {
			t.Fatalf("update kind/type = %+v", u)
		}
	}

	// 新窗口 = 新 seq = 新 batch。
	ds.queueChange("src/a.ts", "/src/a.ts")
	ds.flushChanges()
	batch = nextBatch(t, ch)
	if batch.Seq != 2 || len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/A.js" {
		t.Fatalf("second batch = %+v", batch)
	}
}

// 删除：prune 与"重新执行 importer"在同一条 batch；原子保存产生的
// Remove+Create 必须撤销 prune，且图节点/importers 不能被提前删掉。
func TestHMRPruneAndAtomicSave(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import "./App.js"; export function render() {}`,
		"src/App.js":  `import { x } from "./gone.ts"; import.meta.hot.accept(function () {}); export const App = x;`,
		"src/gone.ts": `export const x = 1;`,
	})
	for _, p := range []string{"/src/main.ts", "/src/App.js", "/src/gone.ts"} {
		get(p)
	}
	ch := captureBatches(t, ds)

	gonePath := filepath.Join(ds.root, "src", "gone.ts")
	// flush 以 stat 为准，测试也必须真的删掉文件，而不是只发事件。
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	ds.handleWatchEvent(fsnotify.Event{Name: gonePath, Op: fsnotify.Remove})
	if _, ok := ds.Module("src/gone.ts"); !ok {
		t.Fatal("防抖窗口内不应立刻移除图节点（原子保存会撤销 prune）")
	}
	ds.flushChanges()

	batch := nextBatch(t, ch)
	if len(batch.Prunes) != 1 || batch.Prunes[0].Path != "/src/gone.ts" {
		t.Fatalf("prunes = %+v", batch.Prunes)
	}
	if len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" {
		t.Fatalf("updates = %+v", batch.Updates)
	}
	if _, ok := ds.Module("src/gone.ts"); ok {
		t.Fatal("确认删除后图节点仍存在")
	}

	// 原子保存：Remove 后紧接着 Create。prune 撤销，importers 仍能用于规划。
	writeGraphFile(t, ds.root, "src/gone.ts", `export const x = 2;`)
	get("/src/gone.ts") // 重新入图，恢复 App -> gone 的反向索引
	ds.handleWatchEvent(fsnotify.Event{Name: gonePath, Op: fsnotify.Remove})
	ds.queueChange("src/gone.ts", "/src/gone.ts")
	ds.flushChanges()
	batch = nextBatch(t, ch)
	if len(batch.Prunes) != 0 {
		t.Fatalf("原子保存不应 prune: %+v", batch.Prunes)
	}
	if len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" {
		t.Fatalf("原子保存 updates = %+v", batch.Updates)
	}
	if _, ok := ds.Module("src/gone.ts"); !ok {
		t.Fatal("原子保存后图节点不应被删")
	}

	// 入口被删除：没有任何 importer 可冒泡，只能整页刷新。
	mainPath := filepath.Join(ds.root, "src", "main.ts")
	if err := os.Remove(mainPath); err != nil {
		t.Fatal(err)
	}
	ds.handleWatchEvent(fsnotify.Event{Name: mainPath, Op: fsnotify.Remove})
	ds.flushChanges()
	batch = nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Type != "full-reload" {
		t.Fatalf("入口删除 batch = %+v", batch)
	}
}

// C3：边界重新执行时，整条脏链上的本地说明符都要带 ?t=<seq>；
// 不受影响的兄弟分支保持裸说明符，npm 裸导入永远不改写。
func TestHMRVersionedSpecifierChain(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts":    `import "./App.js"; export function render() {}`,
		"src/App.js":     `import { child } from "./Child"; import { bare } from "bare-pkg"; import.meta.hot.accept(function () {}); export default { child: child, bare: bare };`,
		"src/Child.ts":   `import { util } from "./util"; import { util as u2 } from "@/util"; export const child = function () { return util + u2; };`,
		"src/util.ts":    `export const util = 1;`,
		"src/leaf.ts":    `export const leaf = 1;`,
		"src/sibling.ts": `import { leaf } from "./leaf"; export const sibling = leaf;`,
	})
	for _, p := range []string{"/src/main.ts", "/src/App.js", "/src/Child.ts", "/src/util.ts", "/src/leaf.ts", "/src/sibling.ts"} {
		get(p)
	}
	ch := captureBatches(t, ds)
	writeGraphFile(t, ds.root, "src/util.ts", `export const util = 2;`)
	ds.queueChange("src/util.ts", "/src/util.ts")
	ds.flushChanges()

	batch := nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" {
		t.Fatalf("util 变化的边界 = %+v", batch.Updates)
	}
	seq := currentSeq(ds)
	q := "?t=" + strconv.FormatInt(seq, 10)

	app := get("/src/App.js" + q)
	if !strings.Contains(app, `"./Child`+q+`"`) {
		t.Fatalf("App 没有给 Child 加版本号:\n%s", app)
	}
	if !strings.Contains(app, `"bare-pkg"`) {
		t.Fatalf("npm 裸导入被改写了:\n%s", app)
	}
	child := get("/src/Child.ts" + q)
	if !strings.Contains(child, `"./util`+q+`"`) {
		t.Fatalf("Child 没有给相对 util 加版本号:\n%s", child)
	}
	if !strings.Contains(child, `"/src/util.ts`+q+`"`) {
		t.Fatalf("Child 没有把别名 util 改写成带版本号的 dev URL:\n%s", child)
	}
	// leaf 也变了？没有：sibling 的依赖不在脏链上，说明符保持原样。
	sib := get("/src/sibling.ts")
	if strings.Contains(sib, "?t=") || !strings.Contains(sib, `"./leaf"`) {
		t.Fatalf("无关分支被误改写:\n%s", sib)
	}
}

// 依赖被删除后已经解析不到，但边界重新执行时仍必须给旧说明符加 ?t；
// 否则浏览器会命中删除前的模块缓存，删除操作看起来"什么都没发生"。
func TestHMRVersionsDeletedDependencyFromPreviousGraph(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import "./App.js"; export function render() {}`,
		"src/App.js":  `import { x } from "./gone.ts"; import.meta.hot.accept(function () {}); export const App = x;`,
		"src/gone.ts": `export const x = 1;`,
	})
	for _, p := range []string{"/src/main.ts", "/src/App.js", "/src/gone.ts"} {
		get(p)
	}
	ch := captureBatches(t, ds)
	gonePath := filepath.Join(ds.root, "src", "gone.ts")
	if err := os.Remove(gonePath); err != nil {
		t.Fatal(err)
	}
	ds.handleWatchEvent(fsnotify.Event{Name: gonePath, Op: fsnotify.Remove})
	ds.flushChanges()

	batch := nextBatch(t, ch)
	if len(batch.Prunes) != 1 || batch.Prunes[0].Path != "/src/gone.ts" {
		t.Fatalf("prunes = %+v", batch.Prunes)
	}
	if len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" || batch.Updates[0].Mode != "reload" {
		t.Fatalf("updates = %+v", batch.Updates)
	}
	q := "?t=" + strconv.FormatInt(currentSeq(ds), 10)
	app := get("/src/App.js" + q)
	if !strings.Contains(app, `"./gone.ts`+q+`"`) {
		t.Fatalf("删除后的依赖没有补版本号（浏览器会命中旧缓存）:\n%s", app)
	}
}

// 说明符版本化的各种形态：相对/绝对只加 query，别名换成带 basePath 的
// canonical URL，query/hash 保留，npm 裸导入与外部 URL 不动。
func TestVersionedSpecifierForms(t *testing.T) {
	ds := &DevServer{root: t.TempDir(), basePath: "/admin"}
	ds.graph = module.NewGraph()
	cases := []struct {
		spec     string
		resolved string
		external bool
		want     string
	}{
		{"./Child", "src/Child.ts", false, "./Child?t=7"},
		{"./data.json?raw", "src/data.json", false, "./data.json?raw&t=7"},
		{"./x.ts#frag", "src/x.ts", false, "./x.ts?t=7#frag"},
		{"@/util", "src/util.ts", false, "/admin/src/util.ts?t=7"},
		{"@theme/x.css", "theme/x.css", false, "/admin/@fs/theme/x.css?t=7"},
		{"bare-pkg", "", true, "bare-pkg"},
		{"https://cdn/x.js", "", true, "https://cdn/x.js"},
	}
	for _, c := range cases {
		got := ds.versionedSpecifier(c.spec, module.Import{Specifier: c.spec, Resolved: c.resolved, External: c.external}, 7)
		if got != c.want {
			t.Errorf("versionedSpecifier(%q, %q) = %q, want %q", c.spec, c.resolved, got, c.want)
		}
	}
	// version <= 0 原样返回。
	if got := ds.versionedSpecifier("./Child", module.Import{Resolved: "src/Child.ts"}, 0); got != "./Child" {
		t.Fatalf("version 0 不应改写: %q", got)
	}
}

type testAdapter struct{}

func (testAdapter) Name() string            { return "svelte" }
func (testAdapter) Extensions() []string    { return []string{"svelte"} }
func (testAdapter) SelfAccepts(string) bool { return true }
func (testAdapter) HMRKind(string) string   { return "svelte" }

// WithAdapters 追加到默认 Vue 适配器之后：扩展名、边界、HMR kind 与
// connected 能力表都要生效，核心代码不需要认识具体框架。
func TestWithAdaptersIntegration(t *testing.T) {
	ds, err := NewDevServer(t.TempDir(), WithAdapters(testAdapter{}))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if !ds.isWatchSource(".svelte") || !ds.isWatchSource(".vue") {
		t.Fatal("扩展名没有被适配器接管")
	}
	if !ds.isSelfAccepting("src/Widget.svelte") {
		t.Fatal("自定义适配器的 self-accept 未生效")
	}
	if kind := ds.adapters.HMRKind("src/Widget.svelte"); kind != "svelte" {
		t.Fatalf("HMR kind = %q", kind)
	}
	hello := ds.connectedMessage()
	if !hello.Capabilities["svelte"] || !hello.Capabilities["vue"] || !hello.Capabilities["batch"] {
		t.Fatalf("connected capabilities = %+v", hello.Capabilities)
	}
	if names := ds.adapters.Names(); strings.Join(names, ",") != "svelte,vue" {
		t.Fatalf("adapter names = %v", names)
	}
}

// 源 CSS 是 self-accept 边界；广播类型是 css，kind 也是 css。
func TestHMRCSSSelfAccept(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts":   `import "./theme.css"; export function render() {}`,
		"src/theme.css": `body { color: red; }`,
	})
	get("/src/main.ts")
	css := get("/src/theme.css")
	for _, want := range []string{"createHotContext", "import.meta.hot.accept()", "data-wf-id"} {
		if !strings.Contains(css, want) {
			t.Fatalf("CSS 产物缺少 %q:\n%s", want, css)
		}
	}
	node, ok := ds.Module("src/theme.css")
	if !ok || !node.SelfAccept || node.HMRKind != "css" {
		t.Fatalf("CSS 节点 = %+v, ok=%v", node, ok)
	}
	ch := captureBatches(t, ds)
	writeGraphFile(t, ds.root, "src/theme.css", `body { color: blue; }`)
	ds.queueChange("src/theme.css", "/src/theme.css")
	ds.flushChanges()
	batch := nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Type != "css" || batch.Updates[0].Kind != "css" {
		t.Fatalf("CSS batch = %+v", batch.Updates)
	}
}

// 依赖冒泡到 Vue 边界必须带 mode=reload：setup 要重新执行，否则旧闭包
// 会继续引用旧依赖，C3 给新依赖加的 ?t 永远不会生效。只有边界自己的
// 直接改动（模板/样式）才能走 rerender。同一批次二者都有时 reload 优先。
func TestHMRIndirectBoundaryUsesReloadMode(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	addNode(ds, "src/App.vue", module.KindVue, true, "vue", imp("./util", "src/util.ts"))
	addNode(ds, "src/util.ts", module.KindJS, false, "")

	batch := ds.buildBatch(5, []pendingChange{{id: "src/util.ts", url: "/src/util.ts"}}, nil)
	if batch == nil || len(batch.Updates) != 1 {
		t.Fatalf("indirect batch = %+v", batch)
	}
	if u := batch.Updates[0]; u.Path != "/src/App.vue" || u.Mode != "reload" {
		t.Fatalf("indirect update = %+v", u)
	}

	batch = ds.buildBatch(6, []pendingChange{{id: "src/App.vue", url: "/src/App.vue"}}, nil)
	if batch == nil || len(batch.Updates) != 1 || batch.Updates[0].Mode != "" {
		t.Fatalf("direct update = %+v", batch)
	}

	batch = ds.buildBatch(7, []pendingChange{
		{id: "src/App.vue", url: "/src/App.vue"},
		{id: "src/util.ts", url: "/src/util.ts"},
	}, nil)
	if batch == nil || len(batch.Updates) != 1 || batch.Updates[0].Mode != "reload" {
		t.Fatalf("mixed update = %+v", batch)
	}
}

// 删除链上的两个模块不能产生"prune 一个已删模块 + update 同一个已删模块"，
// 那会让客户端 import 一个 404；没有可重新执行的边界时整页刷新。
func TestHMRDeletedChainNeverUpdatesPrunedBoundary(t *testing.T) {
	ds, err := NewDevServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	addNode(ds, "src/App.vue", module.KindVue, true, "vue", imp("./Gone.vue", "src/Gone.vue"))
	addNode(ds, "src/Gone.vue", module.KindVue, true, "vue")

	// flush 先移除被删节点，再规划；这里模拟同一时刻。
	ds.graph.Remove("src/Gone.vue")
	ds.graph.Remove("src/App.vue")
	batch := ds.buildBatch(1, nil, []pendingPrune{
		{id: "src/Gone.vue", url: "/src/Gone.vue", importers: []string{"src/App.vue"}},
		{id: "src/App.vue", url: "/src/App.vue"},
	})
	if batch == nil || len(batch.Updates) != 1 || batch.Updates[0].Type != "full-reload" {
		t.Fatalf("deleted chain = %+v", batch)
	}
	if len(batch.Prunes) != 0 {
		t.Fatalf("full-reload 应清空 prunes: %+v", batch.Prunes)
	}
}

// flush 以文件系统为准：文件还在的 Remove 事件是 change，文件没了的
// Change 事件是 prune。这样 Create/Remove 乱序也不会把 pending 归错类。
func TestHMRFlushStatsOverrideEventType(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import "./App.js"; export function render() {}`,
		"src/App.js":  `import { x } from "./util.ts"; import.meta.hot.accept(function () {}); export const App = x;`,
		"src/util.ts": `export const x = 1;`,
	})
	for _, p := range []string{"/src/main.ts", "/src/App.js", "/src/util.ts"} {
		get(p)
	}
	ch := captureBatches(t, ds)

	// 事件说"删了"，但文件还在 → 按 change 处理。
	ds.queuePrune("src/util.ts", "/src/util.ts", []string{"src/App.js"})
	ds.flushChanges()
	batch := nextBatch(t, ch)
	if len(batch.Prunes) != 0 || len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" {
		t.Fatalf("existing file pruned: %+v", batch)
	}

	// 事件说"改了"，但文件已经没了 → 按 prune 处理，并从图里补 importers。
	if err := os.Remove(filepath.Join(ds.root, "src", "util.ts")); err != nil {
		t.Fatal(err)
	}
	ds.queueChange("src/util.ts", "/src/util.ts")
	ds.flushChanges()
	batch = nextBatch(t, ch)
	if len(batch.Prunes) != 1 || batch.Prunes[0].Path != "/src/util.ts" ||
		len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/App.js" {
		t.Fatalf("missing file changed: %+v", batch)
	}
}

// 首次加载就编译失败的模块从未入图；修好后的变更必须发 full-reload，
// 否则 batch 为 nil，错误覆盖层永远等不到更新。
func TestHMRRecoversFromInitialCompileFailure(t *testing.T) {
	ds, _ := newHMRServer(t, map[string]string{
		"src/main.ts": `const broken = ;`,
	})
	rec := doGet(ds.Handler(), "/src/main.ts")
	if rec.Code != 500 {
		t.Fatalf("broken entry = %d: %s", rec.Code, rec.Body.String())
	}
	if !ds.isFailed("src/main.ts") {
		t.Fatal("编译失败没有进入 failed 集合")
	}
	ch := captureBatches(t, ds)
	writeGraphFile(t, ds.root, "src/main.ts", `export function render() {}`)
	ds.queueChange("src/main.ts", "/src/main.ts")
	ds.flushChanges()
	batch := nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Type != "full-reload" {
		t.Fatalf("修复后的 batch = %+v", batch)
	}

	// 成功服务后 failed 清空，后续改动回到正常的入口 full-reload 语义。
	get := func(urlPath string) {
		r := doGet(ds.Handler(), urlPath)
		if r.Code != 200 {
			t.Fatalf("GET %s = %d", urlPath, r.Code)
		}
	}
	get("/src/main.ts")
	if ds.isFailed("src/main.ts") {
		t.Fatal("成功编译后 failed 未清空")
	}
}

// 浏览器请求的是无扩展名 URL（/src/util），文件事件是 src/util.ts。
// 404 记在 /src/util 上；恢复 util.ts 时必须能匹配并 full-reload。
func TestHMRRecoversExtensionlessFailedRequest(t *testing.T) {
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import "./App.js"; export function render() {}`,
		"src/App.js":  `import { x } from "./util"; import.meta.hot.accept(function () {}); export const App = x;`,
		"src/util.ts": `export const x = 1;`,
	})
	get("/src/main.ts")
	get("/src/App.js")
	get("/src/util.ts")
	if r := doGet(ds.Handler(), "/src/util"); r.Code != 302 || r.Header().Get("Location") != "/src/util.ts" {
		t.Fatalf("extensionless GET = %d %q", r.Code, r.Header().Get("Location"))
	}
	if r := doGet(ds.Handler(), "/src/util.ts"); r.Code != 200 {
		t.Fatalf("canonical util GET = %d", r.Code)
	}
	ch := captureBatches(t, ds)
	utilPath := filepath.Join(ds.root, "src", "util.ts")
	if err := os.Remove(utilPath); err != nil {
		t.Fatal(err)
	}
	ds.handleWatchEvent(fsnotify.Event{Name: utilPath, Op: fsnotify.Remove})
	ds.flushChanges()
	_ = nextBatch(t, ch)

	// 边界重新执行时浏览器会请求版本化的无扩展名 URL，此时文件已删。
	if r := doGet(ds.Handler(), "/src/util?t=2"); r.Code != 404 {
		t.Fatalf("deleted util = %d", r.Code)
	}
	// 边界重新执行会重新服务 App.js：此时它的 util import 解析不到，
	// 旧的 App.js → util 边随之消失，util 变成无 importer 的失败模块。
	if r := doGet(ds.Handler(), "/src/App.js?t=2"); r.Code != 200 {
		t.Fatalf("re-serve App.js = %d", r.Code)
	}
	writeGraphFile(t, ds.root, "src/util.ts", `export const x = 2;`)
	ds.queueChange("src/util.ts", "/src/util.ts")
	ds.flushChanges()
	batch := nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Type != "full-reload" {
		t.Fatalf("恢复 util.ts 的 batch = %+v", batch)
	}
}

// Vue dev 产物必须带 __hmrId / __file / accept，并且边界就是组件自己；
// 入口变化仍然整页刷新。依赖真实 wf_modules（没有安装则跳过）。
func TestHMRVueOutputAndBoundary(t *testing.T) {
	mods, err := filepath.Abs("../_examples/basic/web/wf_modules")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mods, ".store", "manifest.json")); err != nil {
		t.Skip("wf_modules 未安装，跳过（先跑 wf install）")
	}
	ds, get := newHMRServer(t, map[string]string{
		"src/main.ts": `import App from "./App.vue"; export const render = function () {};`,
		"src/App.vue": `<template><Counter /></template>
<script setup>
import Counter from './Counter.vue'
</script>`,
		"src/Counter.vue": `<template><button @click="n++">{{ n }}</button></template>
<script setup>
import { ref } from 'vue'
const n = ref(0)
</script>`,
	}, WithModulesDir(mods))

	app := get("/src/App.vue")
	for _, want := range []string{
		"__script.__hmrId", "__script.__file", "import.meta.hot.accept",
		"__VUE_HMR_RUNTIME__", "runtime.reload", `__wfHotCreate("/src/App.vue")`,
	} {
		if !strings.Contains(app, want) {
			t.Fatalf("App.vue dev 产物缺少 %q:\n%s", want, app)
		}
	}
	counter := get("/src/Counter.vue")
	if !strings.Contains(counter, "runtime.reload") {
		t.Fatalf("Counter.vue 缺少 HMR 回调:\n%s", counter)
	}

	appNode, _ := ds.Module("src/App.vue")
	if !appNode.SelfAccept || appNode.HMRKind != "vue" {
		t.Fatalf("App.vue 节点 = %+v", appNode)
	}
	if got := ds.Graph().Importers("src/Counter.vue"); len(got) != 1 || got[0] != "src/App.vue" {
		t.Fatalf("Counter importers = %v", got)
	}
	if got, full := ds.planBoundaries("src/Counter.vue"); full != "" || strings.Join(got, ",") != "src/Counter.vue" {
		t.Fatalf("Counter boundaries = %v, full=%q", got, full)
	}
	ch := captureBatches(t, ds)
	writeGraphFile(t, ds.root, "src/Counter.vue", `<template><button @click="n++">count {{ n }}</button></template>
<script setup>
import { ref } from 'vue'
const n = ref(0)
</script>`)
	batch := nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Path != "/src/Counter.vue" ||
		batch.Updates[0].Kind != "vue" || batch.Updates[0].Type != "update" {
		t.Fatalf("Vue batch = %+v", batch.Updates)
	}
	// 只改模板：重新编译时必须选 rerender，Vue 才不会重建组件实例。
	counter2 := get("/src/Counter.vue?t=2")
	if !strings.Contains(counter2, "runtime.rerender(") {
		t.Fatalf("模板改动应生成 rerender:\n%s", counter2)
	}

	get("/src/main.ts")
	writeGraphFile(t, ds.root, "src/main.ts",
		`import App from "./App.vue"; export const render = function () { console.log("changed") };`)
	batch = nextBatch(t, ch)
	if len(batch.Updates) != 1 || batch.Updates[0].Type != "full-reload" {
		t.Fatalf("入口变化 = %+v", batch.Updates)
	}
}
