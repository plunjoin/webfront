//go:build dev

// hmr.go：HMR 协议 v2 的服务端部分 —— 批次消息、边界冒泡、删除 prune
// 与说明符版本化（C1/C2/C3/C6）。
//
// 一次防抖窗口内的所有文件事件只产生一条 batch：seq 单调递增，updates 里
// 是"需要重新 import 的边界"，prunes 里是被删除的 canonical URL。找不到
// 边界（抵达入口、孤儿模块、环）时退化成一条 full-reload。
package dev

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/plunjoin/webfront/module"
)

const hmrProtocol = 2

// hmrAcceptPattern 识别用户手写的 self-accept（import.meta.hot?.accept）。
// Vue 与 CSS 由 adapter / 内置规则单独标记，不依赖这段文本扫描。
var hmrAcceptPattern = regexp.MustCompile(`import\s*\.\s*meta\s*\.\s*hot\s*\??\.\s*accept`)

type pendingChange struct {
	id  string
	url string
}

type pendingPrune struct {
	id        string
	url       string
	importers []string
}

type hmrConnected struct {
	Type         string          `json:"type"`
	Protocol     int             `json:"protocol"`
	BuildID      string          `json:"buildId,omitempty"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
}

type hmrBatch struct {
	Type      string      `json:"type"`
	Seq       int64       `json:"seq"`
	BuildID   string      `json:"buildId,omitempty"`
	Timestamp int64       `json:"timestamp,omitempty"`
	Updates   []hmrUpdate `json:"updates"`
	Prunes    []hmrPrune  `json:"prunes,omitempty"`
}

type hmrUpdate struct {
	Type    string `json:"type"`
	Path    string `json:"path,omitempty"`
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Version int64  `json:"version,omitempty"`
	Mode    string `json:"mode,omitempty"` // "reload" for indirect/prune updates; empty = auto
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type hmrPrune struct {
	ID   string `json:"id"`
	Path string `json:"path,omitempty"`
}

func (d *DevServer) connectedMessage() hmrConnected {
	caps := map[string]bool{"batch": true, "css": true}
	for _, name := range d.adapters.Names() {
		caps[name] = true
	}
	return hmrConnected{
		Type:         "connected",
		Protocol:     hmrProtocol,
		BuildID:      d.buildID,
		Capabilities: caps,
	}
}

// markDirtyLocked 把一次变更向上冒泡给所有 importers。这样边界重新执行时，
// 不仅边界自己的 URL 带 ?t，链路上每个"下游确实变了"的模块也能拿到新 URL；
// 没有变化的兄弟分支仍是裸 URL，浏览器复用缓存，子组件状态不会被误伤。
func (d *DevServer) markDirtyLocked(id string, seq int64) {
	queue := []string{id}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		key := module.Key(cur)
		if seen[key] {
			continue
		}
		seen[key] = true
		if d.dirty[key] < seq {
			d.dirty[key] = seq
		}
		queue = append(queue, d.graph.Importers(cur)...)
	}
}

// dirtySnapshot 在编译一个模块前取一次快照，插件回调里不再碰锁。
func (d *DevServer) dirtySnapshot() map[string]int64 {
	d.hmrMu.RLock()
	defer d.hmrMu.RUnlock()
	if len(d.dirty) == 0 {
		return nil
	}
	out := make(map[string]int64, len(d.dirty))
	for k, v := range d.dirty {
		out[k] = v
	}
	return out
}

// versionedSpecifier 给"已失效的本地依赖"加 ?t=<seq>。
//
// 相对/绝对说明符只加 query，浏览器仍能相对当前模块 URL 解析；别名说明符
// 改写成 canonical dev URL（import map 对带 query 的裸说明符匹配不一定可靠）。
// npm 裸导入与项目外文件永远不改写，它们由 import map / 浏览器负责。
func (d *DevServer) versionedSpecifier(spec string, imp module.Import, version int64) string {
	if version <= 0 || imp.Resolved == "" || imp.External {
		return spec
	}
	// CSS 的 @import / url() 最终由 <style> 解析，加 ?t 不会换取新的模块
	// 实例，反而可能破坏资源路径；它们由 CSS 自身的 self-accept 处理。
	switch imp.Kind {
	case "import-rule", "url-token", "composes-from":
		return spec
	}
	q := "t=" + strconv.FormatInt(version, 10)
	isLocal := strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || filepath.IsAbs(spec)
	if isLocal {
		return appendSpecifierQuery(spec, q)
	}

	// 别名：解析到文件后换成 dev URL，保留原说明符上的 ?raw / ?url / hash。
	u, ok := d.devURL(filepath.Join(d.root, filepath.FromSlash(imp.Resolved)))
	if !ok {
		return spec
	}
	suffix, hash := "", ""
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		suffix = spec[i:]
	}
	if i := strings.IndexByte(suffix, '#'); i >= 0 {
		hash = suffix[i:]
		suffix = suffix[:i]
	}
	if strings.HasPrefix(suffix, "?") {
		return u + suffix + "&" + q + hash
	}
	return u + "?" + q + hash
}

func appendSpecifierQuery(spec, q string) string {
	hash := ""
	if i := strings.IndexByte(spec, '#'); i >= 0 {
		hash = spec[i:]
		spec = spec[:i]
	}
	if strings.Contains(spec, "?") {
		return spec + "&" + q + hash
	}
	return spec + "?" + q + hash
}

func (d *DevServer) entryID() string {
	p := strings.TrimPrefix(d.entry, d.basePath)
	p = strings.TrimPrefix(p, "/")
	return p
}

func (d *DevServer) isEntry(id string) bool {
	e := d.entryID()
	return e != "" && module.Key(id) == module.Key(e)
}

// isSelfAccepting：适配器（Vue）、内置 CSS、或用户手写 accept 的模块。
func (d *DevServer) isSelfAccepting(id string) bool {
	if d.adapters.SelfAccepts(id) {
		return true
	}
	if strings.EqualFold(path.Ext(id), ".css") {
		return true
	}
	if node, ok := d.graph.Node(id); ok && node.SelfAccept {
		return true
	}
	return false
}

// planBoundaries 返回需要重新 import 的边界；full 非空表示必须整页刷新。
// 空切片 + 空 full 表示这个模块还没进过浏览器，忽略即可。
func (d *DevServer) planBoundaries(id string) ([]string, string) {
	_, loaded := d.graph.Node(id)
	importers := d.graph.Importers(id)
	if !loaded && len(importers) == 0 {
		// 首次加载就编译失败的模块从未入图。修好它时如果没有这条兜底，
		// batch 会是 nil，错误覆盖层永远等不到下一次更新。
		if d.isFailed(id) {
			return nil, "module recovered from a compile error: " + id
		}
		return nil, ""
	}
	if d.isSelfAccepting(id) {
		return []string{id}, ""
	}
	if d.isEntry(id) {
		return nil, "entry module changed: " + id
	}
	if len(importers) == 0 {
		return nil, "no HMR boundary for " + id
	}
	return d.bubble(importers, map[string]bool{module.Key(id): true}, id)
}

// planFromImporter 处理删除：被删模块自己无法重新 import，从它的 importer
// 开始找边界。importer 自己 self-accept 时就是边界（例如 App.vue 引用了
// 一个被删的 util，重新执行 App.vue 会得到真实的 import 错误覆盖层）。
func (d *DevServer) planFromImporter(id string) ([]string, string) {
	if d.isSelfAccepting(id) {
		return []string{id}, ""
	}
	if d.isEntry(id) {
		return nil, "entry imports a deleted module: " + id
	}
	importers := d.graph.Importers(id)
	if len(importers) == 0 {
		return nil, "deleted module importer has no boundary: " + id
	}
	return d.bubble(importers, map[string]bool{module.Key(id): true}, id)
}

// bubble 沿 importers 向上做 BFS。visited 让环收敛：环里没有边界时队列自然
// 耗尽，最终退化成 full reload，不会挂死。任意一条路径抵达入口或死胡同都
// 保守地整页刷新（比"只更新一半边界"可预期）。
func (d *DevServer) bubble(start []string, visited map[string]bool, origin string) ([]string, string) {
	boundaries := map[string]bool{}
	queue := append([]string(nil), start...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		key := module.Key(cur)
		if visited[key] {
			continue
		}
		visited[key] = true
		if d.isSelfAccepting(cur) {
			boundaries[cur] = true
			continue
		}
		if d.isEntry(cur) {
			return nil, "entry reached from " + origin + ": " + cur
		}
		importers := d.graph.Importers(cur)
		if len(importers) == 0 {
			return nil, "no HMR boundary for " + origin + " (reached " + cur + ")"
		}
		queue = append(queue, importers...)
	}
	if len(boundaries) == 0 {
		return nil, "no HMR boundary for " + origin
	}
	out := make([]string, 0, len(boundaries))
	for id := range boundaries {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, ""
}

func (d *DevServer) buildBatch(seq int64, changes []pendingChange, prunes []pendingPrune) *hmrBatch {
	batch := &hmrBatch{
		Type:      "batch",
		Seq:       seq,
		BuildID:   d.buildID,
		Timestamp: time.Now().UnixMilli(),
		Updates:   []hmrUpdate{},
	}
	updates := map[string]hmrUpdate{}
	changed := map[string]bool{}
	pruned := map[string]bool{}
	for _, c := range changes {
		changed[module.Key(c.id)] = true
	}
	for _, p := range prunes {
		pruned[module.Key(p.id)] = true
	}

	fullReason := ""
	for _, c := range changes {
		boundaries, full := d.planBoundaries(c.id)
		if full != "" {
			fullReason = full
			break
		}
		for _, b := range boundaries {
			// 直接改动边界自己 → Vue 可以 rerender；依赖冒泡过来的
			// 更新必须 reload，否则 setup 闭包不会重新执行、拿不到新依赖。
			d.addBoundaryUpdate(updates, b, module.Key(b) == module.Key(c.id), pruned)
		}
	}

	if fullReason == "" {
		for _, p := range prunes {
			// 原子保存的 Remove 已被同名 Create 撤销。
			if changed[module.Key(p.id)] {
				continue
			}
			if d.isEntry(p.id) {
				fullReason = "entry module deleted: " + p.id
				break
			}
			if p.url != "" {
				batch.Prunes = append(batch.Prunes, hmrPrune{ID: p.id, Path: p.url})
			}
			for _, imp := range p.importers {
				if pruned[module.Key(imp)] {
					// 删除链上的 importer 也被删了：没有可重新执行的模块，
					// 只更新一半边界不如整页刷新可预期。
					fullReason = "deleted module chain: " + p.id
					break
				}
				boundaries, full := d.planFromImporter(imp)
				if full != "" {
					fullReason = full
					break
				}
				for _, b := range boundaries {
					// 删除永远不是"边界自己变了"：Vue 必须重跑 setup，
					// 重新 import 才能暴露真实的 unresolved-import 错误。
					d.addBoundaryUpdate(updates, b, false, pruned)
				}
			}
			if fullReason != "" {
				break
			}
		}
	}

	if fullReason != "" {
		batch.Updates = []hmrUpdate{{Type: "full-reload", Reason: fullReason}}
		batch.Prunes = nil
		return batch
	}
	if len(updates) == 0 && len(batch.Prunes) == 0 {
		return nil
	}
	batch.Updates = make([]hmrUpdate, 0, len(updates))
	for _, u := range updates {
		batch.Updates = append(batch.Updates, u)
	}
	sort.Slice(batch.Updates, func(i, j int) bool { return batch.Updates[i].Path < batch.Updates[j].Path })
	sort.Slice(batch.Prunes, func(i, j int) bool { return batch.Prunes[i].Path < batch.Prunes[j].Path })
	return batch
}

func (d *DevServer) addBoundaryUpdate(updates map[string]hmrUpdate, id string, direct bool, pruned map[string]bool) {
	// 被删除的模块不能出现在 updates 里：客户端会先 prune 再 import
	// 一个已经不存在的 URL（404）。
	if pruned[module.Key(id)] {
		return
	}
	node, ok := d.graph.Node(id)
	url, kind, version := "", "", int64(0)
	if ok {
		url, kind, version = node.URL, string(node.Kind), node.Version
	}
	if url == "" {
		if u, ok := d.devURL(filepath.Join(d.root, filepath.FromSlash(id))); ok {
			url = u
		}
	}
	if url == "" {
		return
	}
	if hk := d.adapters.HMRKind(id); hk != "" {
		kind = hk
	}
	if kind == "" {
		kind = kindFromID(id)
	}
	typ := "update"
	if kind == "css" {
		typ = "css"
	}
	mode := ""
	if !direct {
		mode = "reload"
	}
	if prev, exists := updates[url]; exists {
		// 同一个边界这次既有直接改动又有依赖冒泡：reload 优先。
		if mode == "reload" {
			prev.Mode = "reload"
		}
		updates[url] = prev
		return
	}
	updates[url] = hmrUpdate{
		Type:    typ,
		Path:    url,
		ID:      id,
		Kind:    kind,
		Version: version,
		Mode:    mode,
	}
}

func kindFromID(id string) string {
	ext := path.Ext(id)
	switch strings.ToLower(ext) {
	case ".vue":
		return "vue"
	case ".css":
		return "css"
	case ".json":
		return "json"
	}
	if isAssetExt(ext) {
		return "asset"
	}
	return "js"
}
