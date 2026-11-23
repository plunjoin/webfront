//go:build dev

// dev.go：DevServer 本体。文件带 dev tag——生产构建 import 本包时只编译
// handler.go / options.go 这些轻量文件，esbuild、fsnotify、websocket 都不会
// 进二进制。
package dev

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"

	"github.com/plunjoin/webfront/adapter"
	"github.com/plunjoin/webfront/compiler"
	"github.com/plunjoin/webfront/module"
)

// DevServer：零 Node 复刻 Vite 极速体验（§3.2）。
//   - 浏览器请求 /src/*.tsx / .vue 时，按需单文件转译（毫秒级）返回 JS
//   - 浏览器请求 npm 裸模块时，通过 <script type="importmap"> 路由到 /@deps/:pkg，按需 esbuild 打包缓存
//   - fsnotify 监听 web/src 变更 → WebSocket(/__wf_hmr) 通知浏览器 import() 热替换
type DevServer struct {
	root       string
	entry      string // 浏览器入口（URL 形式，如 /src/main.tsx）
	watcher    *fsnotify.Watcher
	upgrader   websocket.Upgrader
	mu         sync.Mutex
	clients    map[*wsClient]struct{}
	depsLock   sync.RWMutex
	depsCache  map[string][]byte
	rootDeps   map[string]string
	modulesDir string

	// 每个被服务过的模块的 id / version / imports / importers。
	// 切片 0 只负责建立并暴露这张图；切片 1 用 importers 做边界冒泡，
	// 用 selfAccept/hmrKind 判断哪些模块能就地热替换。
	graph *module.Graph

	// 适配器注册表：框架扩展名、self-accept 与 HMR kind 都由它提供。
	// Vue 默认注册；WithAdapters 可替换。
	adapters *adapter.Registry

	// 每个 .vue 上一次编译的 <script> 哈希。重编译时相等 → 只改了模板/
	// 样式，Vue HMR 走 rerender 保留组件状态；否则走 reload。
	sfcMu     sync.Mutex
	sfcScript map[string]string

	// HMR 会话标识与批次序号。buildId 随 DevServer 启动生成，客户端用它
	// 识别"这条消息来自上一次进程"；seq 是全局单调递增的批次号。
	buildID string

	// 说明符版本化的脏标记：module.Key(id) -> 最后一次变更的 seq。
	// 变更向上冒泡到所有 importers，这样边界重新执行时，链路上每个
	// "下游确实变了"的本地说明符都会带 ?t=<seq>，浏览器不会复用旧模块。
	hmrMu sync.RWMutex
	dirty map[string]int64
	seq   int64

	// 首次加载就编译失败的模块：它们从未入图，修好时没有 importers 可冒泡。
	// 下一次变更必须发 full-reload，页面才能重新走一遍入口 import 并清掉覆盖层。
	failed map[string]bool

	// 一次防抖窗口只允许一个 flush 在跑：并发的 AfterFunc 回调若同时
	// 分配 seq，广播顺序可能与 seq 顺序相反，客户端会把较新的更新当过期丢掉。
	flushMu sync.Mutex

	// 关闭与生命周期。Close 会关闭 watcher、WebSocket 客户端，并等待所有
	// 后台 goroutine 退出；closeOnce 保证重复调用安全。
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	// watcher 防抖：一次保存往往产生多个事件，合并成一个短窗口后再广播。
	// pending/prunes 都以 module.Key(id) 为键：Windows 上大小写不同的同一
	// 路径不能产生两条更新。
	watchMu  sync.Mutex
	pending  map[string]*pendingChange
	prunes   map[string]*pendingPrune
	debounce *time.Timer

	// 挂载前缀（"" 表示根路径）：生成的 URL、import map、HMR 地址都带上它。
	basePath string

	// 开发态静态资源目录（Vite publicDir 约定）：未命中内部路由的请求回退到
	// 这里的文件，仍找不到才回 SPA 外壳。默认 <root>/public，WithPublicDir 可改/关闭。
	publicDir string
	publicSet bool

	// esbuild define 表：默认 NODE_ENV=development，Vue 项目自动补特性开关，
	// 用户通过 WithDefine 追加的项最后覆盖。
	define     map[string]string
	userDefine map[string]string

	// /@deps 打包时外置的包。默认是所有 root 依赖（同一 runtime 只加载一份）；
	// externalSet 记录调用方是否显式设置过（WithExternal() 空列表 = 关闭）。
	externals   map[string]bool
	externalSet bool

	// tsconfig.json 原文：esbuild 的 Transform 不会自动找 tsconfig，
	// 不传的话 `jsx: react-jsx` 会退化成 React.createElement。
	tsconfigRaw string

	// 导入别名（默认 @/ → src/，外加 tsconfig paths）。dev 期不做模块解析，
	// 所以把它们写进 index.html 的 import map，交给浏览器完成解析。
	// 启动时读取一次，之后只读；改 tsconfig 需要重启 dev server。
	aliases []compiler.Alias

	// 最近一次编译错误。浏览器 import() 失败时拿不到是哪个文件坏的
	// （嵌套依赖只会抛一句 "Failed to fetch dynamically imported module"），
	// 所以服务端留一份，客户端通过 /__wf_error?since=N 取回原文。
	errMu   sync.Mutex
	errSeq  int64
	lastErr *compileError

	// 开发态 SSR：entry / factory 来自 WithSSR；bundle 在后台重编译后
	// 原子替换 renderer。ssrMu 的写锁会等在途渲染退出后才关闭旧池。
	ssrEntry    string
	ssrFactory  SSRRendererFactory
	ssrRenderer SSRRenderer
	ssrMu       sync.RWMutex
	ssrTrigger  chan struct{}
	ssrStateMu  sync.Mutex
	ssrLastErr  string
	ssrHadError bool
	ssrBuilds   int64

	// HTML shell：WithHTMLShell 优先，其次 WithHTMLShellFile。
	shell         ShellFunc
	shellFile     string
	shellTemplate []byte
}

// compileError 是一条编译错误的快照，JSON 直接喂给浏览器。
type compileError struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Body string `json:"body"`
	At   string `json:"at"`
}

// markFailed 记录/清除"这个模块最近一次编译失败"。读取方（planBoundaries）
// 与写入方（serveURL）可能在不同请求 goroutine 上，统一用 hmrMu 保护。
func (d *DevServer) markFailed(id string, failed bool) {
	if id == "" {
		return
	}
	key := module.Key(id)
	d.hmrMu.Lock()
	if d.failed == nil {
		d.failed = map[string]bool{}
	}
	if failed {
		d.failed[key] = true
	} else {
		delete(d.failed, key)
	}
	d.hmrMu.Unlock()
}

func (d *DevServer) isFailed(id string) bool {
	key := module.Key(id)
	d.hmrMu.RLock()
	ok := d.failed[key]
	if !ok {
		// 浏览器请求的是无扩展名的 dev URL（./util → /src/util），
		// 文件事件却是 src/util.ts。404 记在无扩展名 key 上，恢复时要
		// 让同 basename 的源文件也能触发 full-reload。
		base := strings.TrimSuffix(key, strings.ToLower(path.Ext(key)))
		if base != key {
			for failedKey := range d.failed {
				if failedKey == base {
					ok = true
					break
				}
			}
		}
	}
	d.hmrMu.RUnlock()
	return ok
}

// recordError 记下这次编译失败（只保留最近一条，够用且不会涨内存）。
func (d *DevServer) recordError(path, body string) {
	d.errMu.Lock()
	d.errSeq++
	d.lastErr = &compileError{
		ID:   d.errSeq,
		Path: path,
		Body: body,
		At:   time.Now().Format("15:04:05"),
	}
	d.errMu.Unlock()
}

// fail 统一处理"这个文件编译不过"：记一笔 + 500 + 可读文本。
// 响应体就是覆盖层要展示的原文，所以这里不要再加多余前缀。
func (d *DevServer) fail(w http.ResponseWriter, path, body string) {
	body = strings.TrimRight(body, "\n")
	d.recordError(path, body)
	http.Error(w, body, http.StatusInternalServerError)
}

// failModule 在 fail 之外标记模块失败：首次加载失败的模块不入图，
// 修好后的变更只能靠 failed 集合触发一次 full-reload。
func (d *DevServer) failModule(w http.ResponseWriter, path, body, id string) {
	d.markFailed(id, true)
	d.fail(w, path, body)
}

// readTSConfigRaw 读取项目根目录的 tsconfig / jsconfig 原文，供 esbuild Transform 使用。
func readTSConfigRaw(root string) string {
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			return string(b)
		}
	}
	return ""
}

func NewDevServer(root string, opts ...Option) (*DevServer, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(root)

	d := &DevServer{
		root:      abs,
		watcher:   w,
		clients:   map[*wsClient]struct{}{},
		depsCache: map[string][]byte{},
		graph:     module.NewGraph(),
		adapters:  adapter.New(),
		buildID:   newBuildID(),
		closed:    make(chan struct{}),
		pending:   map[string]*pendingChange{},
		prunes:    map[string]*pendingPrune{},
		dirty:     map[string]int64{},
		failed:    map[string]bool{},
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	d.applyConfig(newConfig(opts))

	// SSR 与 HTML shell 的配置在启动时就校验：静默退化成 SPA 会让
	// "SSR 没生效" 很难被发现。shell 文件也只读一次，运行中改它需重启。
	if (d.ssrEntry == "") != (d.ssrFactory == nil) {
		_ = w.Close()
		if d.ssrEntry == "" {
			return nil, fmt.Errorf("dev: WithSSR 需要非空的 entry 与 factory（当前 factory 非 nil 但 entry 为空）")
		}
		return nil, fmt.Errorf("dev: WithSSR(%q) 缺少 SSR 渲染器 factory（用 dev/ssr.Factory）", d.ssrEntry)
	}
	if d.shell == nil && d.shellFile != "" {
		shellPath := d.shellFile
		if !filepath.IsAbs(shellPath) {
			shellPath = filepath.Join(abs, filepath.FromSlash(shellPath))
		}
		b, err := os.ReadFile(shellPath)
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("dev: 读取 HTML shell %s: %w", shellPath, err)
		}
		d.shellTemplate = stripProdClientAssets(b)
	}

	// public/：显式 WithPublicDir 优先，否则用 <root>/public（存在才托管）。
	// 路径启动时固定，但每次请求都重新 stat，运行中新建的 public/ 也能生效。
	if !d.publicSet {
		d.publicDir = "public"
	}
	if d.publicDir != "" {
		if !filepath.IsAbs(d.publicDir) {
			d.publicDir = filepath.Join(abs, d.publicDir)
		}
		if fi, err := os.Stat(d.publicDir); err == nil && fi.IsDir() {
			fmt.Printf("[public] serving %s\n", d.publicDir)
		}
	}

	// 依赖目录与 root 依赖：显式 WithModulesDir 优先，否则自底向上探测。
	// rootDeps 同时用于 import map 与 /@deps 的默认外置集合。
	modulesDir := d.modulesDir
	if modulesDir == "" {
		if md, ok := findModulesDir(abs); ok {
			modulesDir = md
		} else {
			modulesDir = "node_modules"
		}
	}
	rootDeps := map[string]string{}
	storeDir := modulesDir
	if !filepath.IsAbs(storeDir) {
		storeDir = filepath.Join(abs, storeDir)
	}
	if m, err := compiler.LoadProjectStoreManifest(filepath.Join(storeDir, ".store"), abs); err == nil {
		for k, v := range m.Root {
			rootDeps[k] = v
		}
	}

	// 补充读取 package.json dependencies
	if b, err := os.ReadFile(filepath.Join(abs, "package.json")); err == nil {
		var pj struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(b, &pj) == nil {
			for k, v := range pj.Dependencies {
				if _, ok := rootDeps[k]; !ok {
					rootDeps[k] = v
				}
			}
		}
	}

	d.modulesDir = modulesDir
	d.rootDeps = rootDeps
	d.define = buildDefine(rootDeps, d.userDefine)
	if !d.externalSet {
		// 默认把所有 root 依赖外置：经由 import map 共享同一份 /@deps/<pkg>.js，
		// react-dom 内联一份 react 造成的 Invalid hook call 就此消失。
		d.externals = map[string]bool{}
		for pkg := range rootDeps {
			d.externals[pkg] = true
		}
	}
	d.entry = detectDevEntry(abs)
	d.aliases = compiler.LoadAliases(abs)
	d.tsconfigRaw = readTSConfigRaw(abs)
	for _, a := range d.aliases {
		if _, _, ok := d.aliasImport(a); !ok {
			fmt.Printf("[alias] 跳过 %s（目标不在项目目录内，dev 无法提供）: %s\n", a.Prefix, a.Target)
			continue
		}
		fmt.Printf("[alias] %s -> %s\n", a.Prefix, a.Target)
	}
	// SSR：先同步编译一次，页面从第一个请求起就能拿到真实 HTML；失败也
	// 不阻塞启动（源码可能正停在半成品状态），保留 nil renderer 并靠下一次
	// 文件变更重试。后续重编译在 ssrLoop 里串行进行。
	if d.ssrEntry != "" {
		d.ssrTrigger = make(chan struct{}, 1)
		d.rebuildSSR(false)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.ssrLoop()
		}()
	}
	// fsnotify 不递归：必须把 src 下每一层子目录都加进监听，
	// 否则改 src/components/Foo.vue 这种文件时 HMR 完全收不到事件。
	d.watchDirs(abs)
	fmt.Printf("[HMR] watching %s (recursive)\n", abs)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.watch()
	}()
	return d, nil
}

// newBuildID 给一次 DevServer 生命周期生成会话标识。浏览器用它区分
// "同一次进程的批次"与"Go 重启前后的旧连接"；失败时退化成纳秒时间戳。
func newBuildID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}

// detectDevEntry 找出浏览器入口（按常见约定排序）。
func detectDevEntry(root string) string {
	for _, p := range []string{
		"src/main.tsx", "src/main.ts", "src/main.jsx", "src/main.js",
		"src/entry-client.tsx", "src/entry-client.ts", "src/entry-client.jsx", "src/entry-client.js",
	} {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err == nil && !fi.IsDir() {
			return "/" + p
		}
	}
	return "/src/main.tsx"
}

// watchDirs 递归监听目录；跳过依赖/产物目录。
func (d *DevServer) watchDirs(root string) {
	_ = filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if p != root && ignoredDir(entry.Name()) {
			return filepath.SkipDir
		}
		if err := d.watcher.Add(p); err != nil {
			fmt.Printf("[HMR] warn: cannot watch %s: %v\n", p, err)
		}
		return nil
	})
}

func ignoredDir(name string) bool {
	switch name {
	case "node_modules", "wf_modules", "dist", ".git", ".store", "vendor":
		return true
	}
	return false
}

// watch 监听源码变更并广播给所有 HMR 客户端。
//
// watcher.Errors 必须被消费：fsnotify 的 errors channel 是无缓冲的，长期不读
// 会让 watcher 自己阻塞。删除/改名时先移除图节点，避免 imports/importers
// 指向一个已经不存在的模块。
func (d *DevServer) watch() {
	for {
		select {
		case <-d.closed:
			return
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			d.handleWatchEvent(ev)
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			if err != nil {
				fmt.Printf("[HMR] watcher error: %v\n", err)
			}
		}
	}
}

func (d *DevServer) handleWatchEvent(ev fsnotify.Event) {
	switch {
	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		id, ok := module.CanonicalID(d.root, ev.Name)
		if !ok {
			return
		}
		// 图节点先留着：原子保存（写 .tmp → rename 覆盖）会先发 Remove
		// 再发 Create；如果此刻就 Remove 节点，取消 prune 后 importers
		// 已经丢了，下一次改动就找不到边界。真正删除在防抖窗口结束、
		// 确认没有被 Create 撤销后再做。
		node, hadNode := d.graph.Node(id)
		var importers []string
		if hadNode {
			importers = node.Importers
		}
		if hadNode || len(importers) > 0 {
			url, _ := d.devURL(ev.Name)
			d.queuePrune(id, url, importers)
			fmt.Printf("[HMR] removed %s (pending)\n", id)
		}
		return
	case ev.Op&(fsnotify.Write|fsnotify.Create) == 0:
		// 改名式保存（写 .tmp 再 rename 覆盖）在 Windows 上会先发 Remove(旧文件)
		// 再 Create(新文件)。Remove 只入 prune；等 Create/Write 到来会覆盖它。
		return
	}

	if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
		// 新建目录（如 src/components）也要纳入监听，否则后续改动收不到。
		d.watchDirs(ev.Name)
		return
	}
	if !d.isWatchSource(filepath.Ext(ev.Name)) {
		return
	}
	// 用 devURL 而不是拼 "/"+相对路径：src/ 下的文件映射到 /src/，
	// 别名指向的其他目录（shared/ 等）映射到 /@fs/，否则浏览器 import 会撞上 index.html。
	url, ok := d.devURL(ev.Name)
	if !ok {
		return
	}
	id, ok := module.CanonicalID(d.root, ev.Name)
	if !ok {
		return
	}
	d.queueChange(id, url)
}

// isWatchSource 由内置类型 + adapter 声明的扩展名共同决定。
// .vue 不再写死在这里：默认注册的 adapter/vue 会声明它，核心只认接口。
func (d *DevServer) isWatchSource(ext string) bool {
	switch strings.ToLower(ext) {
	case ".tsx", ".ts", ".jsx", ".js", ".mjs", ".cjs", ".css", ".json":
		return true
	}
	for _, e := range d.adapters.Extensions() {
		if strings.EqualFold(ext, e) {
			return true
		}
	}
	return false
}

// queueChange 合并 100ms 窗口内的所有事件：一次保存通常同时产生 Write 与
// Create，逐个广播会让浏览器重复 import 同一个模块。
func (d *DevServer) queueChange(id, url string) {
	if d.isClosed() {
		return
	}
	d.watchMu.Lock()
	if d.pending == nil {
		d.pending = map[string]*pendingChange{}
	}
	key := module.Key(id)
	d.pending[key] = &pendingChange{id: id, url: url}
	// 原子保存会先 Remove 再 Create；文件又回来了，prune 必须撤销。
	delete(d.prunes, key)
	d.armFlushLocked()
	d.watchMu.Unlock()
	fmt.Printf("[HMR] queued %s\n", id)
}

// queuePrune 记录一次删除/改名。真正广播要等防抖窗口结束：编辑器可能只是
// 用 rename 落盘，紧接着的 Create 会把这条 prune 撤销。
func (d *DevServer) queuePrune(id, url string, importers []string) {
	if d.isClosed() {
		return
	}
	d.watchMu.Lock()
	if d.prunes == nil {
		d.prunes = map[string]*pendingPrune{}
	}
	key := module.Key(id)
	d.prunes[key] = &pendingPrune{id: id, url: url, importers: append([]string(nil), importers...)}
	delete(d.pending, key)
	d.armFlushLocked()
	d.watchMu.Unlock()
}

func (d *DevServer) armFlushLocked() {
	if d.debounce == nil {
		d.debounce = time.AfterFunc(100*time.Millisecond, d.flushChanges)
	} else {
		d.debounce.Reset(100 * time.Millisecond)
	}
}

func (d *DevServer) flushChanges() {
	// 整个 drain → stat → seq → 规划 → 广播 必须串行：两个定时器回调同时
	// 分配 seq 时，广播顺序可能与 seq 相反，客户端会把新更新当过期丢掉。
	d.flushMu.Lock()
	defer d.flushMu.Unlock()

	d.watchMu.Lock()
	changes := make([]pendingChange, 0, len(d.pending))
	for _, c := range d.pending {
		changes = append(changes, *c)
	}
	prunes := make([]pendingPrune, 0, len(d.prunes))
	for _, p := range d.prunes {
		prunes = append(prunes, *p)
	}
	d.pending = map[string]*pendingChange{}
	d.prunes = map[string]*pendingPrune{}
	if d.debounce != nil {
		// 手动 flush（测试/关闭前）与定时器赛跑时，先把定时器停掉；
		// 定时器自己触发时 Stop 返回 false，也没有副作用。
		d.debounce.Stop()
		d.debounce = nil
	}
	d.watchMu.Unlock()

	if d.isClosed() || (len(changes) == 0 && len(prunes) == 0) {
		return
	}

	// 以 flush 时刻的文件系统状态为准，而不是事件类型：Windows 的原子保存
	// 可能只观察到 Remove+Create 中的一半，Create 后紧跟 Remove 也会把
	// pending 归错类。一次 stat 就能把 change/prune 摆正。
	changes, prunes = d.reconcileFlush(changes, prunes)
	if len(changes) == 0 && len(prunes) == 0 {
		return
	}
	// SSR bundle 也要跟着重编译：即使某个源文件还没被浏览器请求过，
	// 它也可能改变 entry-server 的产物。trigger 是容量 1 的非阻塞信号，
	// 编译期间的多次保存会合并成下一次重建。
	d.kickSSR()
	sort.Slice(changes, func(i, j int) bool { return changes[i].id < changes[j].id })
	sort.Slice(prunes, func(i, j int) bool { return prunes[i].id < prunes[j].id })

	// 一个防抖窗口 = 一个 seq = 一条 batch。脏标记先于计划写入，保证
	// 客户端收到 batch 后重新请求边界时，服务端已经能改写说明符。
	d.hmrMu.Lock()
	if d.dirty == nil {
		d.dirty = map[string]int64{}
	}
	d.seq++
	seq := d.seq
	for _, c := range changes {
		d.markDirtyLocked(c.id, seq)
	}
	for _, p := range prunes {
		key := module.Key(p.id)
		delete(d.failed, key)
		if d.dirty[key] < seq {
			d.dirty[key] = seq
		}
		// 删除前抓到的 importers 是唯一线索：图里的反向索引马上会被移除。
		// 先冒泡脏标记（规划边界时还要用 importers），再删节点。
		for _, imp := range p.importers {
			d.markDirtyLocked(imp, seq)
		}
	}
	d.hmrMu.Unlock()

	// prune 已经被同一个防抖窗口里的 Create 撤销过一轮；走到这里的都是
	// 真删除。等 dirty 冒泡完成后再移除节点，规划阶段仍可查询图。
	for _, p := range prunes {
		d.graph.Remove(p.id)
		d.sfcMu.Lock()
		delete(d.sfcScript, module.Key(p.id))
		d.sfcMu.Unlock()
	}

	batch := d.buildBatch(seq, changes, prunes)
	if batch == nil {
		return
	}
	d.broadcast(batch)
	items := len(batch.Updates) + len(batch.Prunes)
	fmt.Printf("[HMR] broadcast seq=%d (%d item(s)) to %d client(s)\n", seq, items, d.clientCount())
}

// reconcileFlush 用 os.Stat 校正防抖窗口内收集到的事件：文件存在就是
// change，不存在就是 prune。事件类型只用于尽快把 id 放进 pending，
// 不再决定最终语义（Windows 原子保存的 Remove/Create 顺序并不可靠）。
func (d *DevServer) reconcileFlush(changes []pendingChange, prunes []pendingPrune) ([]pendingChange, []pendingPrune) {
	exists := func(id string) bool {
		fi, err := os.Stat(filepath.Join(d.root, filepath.FromSlash(id)))
		return err == nil && !fi.IsDir()
	}
	changeByKey := make(map[string]pendingChange, len(changes)+len(prunes))
	pruneByKey := make(map[string]pendingPrune, len(changes)+len(prunes))
	for _, c := range changes {
		key := module.Key(c.id)
		if exists(c.id) {
			changeByKey[key] = c
			continue
		}
		p := pendingPrune{id: c.id, url: c.url}
		if node, ok := d.graph.Node(c.id); ok {
			p.importers = append([]string(nil), node.Importers...)
		}
		pruneByKey[key] = p
	}
	for _, p := range prunes {
		key := module.Key(p.id)
		if exists(p.id) {
			changeByKey[key] = pendingChange{id: p.id, url: p.url}
			continue
		}
		if prev, ok := pruneByKey[key]; ok && len(prev.importers) > 0 {
			p.importers = prev.importers
		}
		pruneByKey[key] = p
	}
	outChanges := make([]pendingChange, 0, len(changeByKey))
	for _, c := range changeByKey {
		outChanges = append(outChanges, c)
	}
	outPrunes := make([]pendingPrune, 0, len(pruneByKey))
	for _, p := range pruneByKey {
		outPrunes = append(outPrunes, p)
	}
	return outChanges, outPrunes
}

func (d *DevServer) clientCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.clients)
}

// wsClient 把"写 WebSocket"从广播路径里拆出来：每个连接一个 writer
// goroutine，慢客户端只丢自己的消息，不会在 d.mu 里卡住整个 dev server。
type wsClient struct {
	conn      *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

func (c *wsClient) writeLoop(d *DevServer) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.closed:
			// 服务端关闭时也要把连接关掉，否则 handler 里的 ReadMessage
			// 会一直阻塞，客户端看不到断开。
			c.close()
			return
		case <-c.done:
			return
		case b := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

func (d *DevServer) broadcast(msg any) {
	b, _ := json.Marshal(msg)
	d.mu.Lock()
	for c := range d.clients {
		select {
		case c.send <- b:
		default:
			// 客户端读得太慢：断开它，页面重连后从头同步，比静默丢消息可预期。
			delete(d.clients, c)
			c.close()
		}
	}
	d.mu.Unlock()
}

func (d *DevServer) removeClient(c *wsClient) {
	d.mu.Lock()
	delete(d.clients, c)
	d.mu.Unlock()
	c.close()
}

func (d *DevServer) isClosed() bool {
	if d.closed == nil {
		return false
	}
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// Close 停止 watcher、断开所有 HMR 客户端并等待后台 goroutine 退出。
// 可重复调用。
func (d *DevServer) Close() error {
	var err error
	d.closeOnce.Do(func() {
		if d.closed != nil {
			close(d.closed)
		}
		d.watchMu.Lock()
		if d.debounce != nil {
			d.debounce.Stop()
			d.debounce = nil
		}
		d.watchMu.Unlock()
		if d.watcher != nil {
			err = d.watcher.Close()
		}
		d.mu.Lock()
		for c := range d.clients {
			c.close()
		}
		d.clients = map[*wsClient]struct{}{}
		d.mu.Unlock()
	})
	d.wg.Wait()
	// ssrLoop 已退出；拿写锁会等在途 RenderHTML 结束，然后才能关旧池。
	d.ssrMu.Lock()
	renderer := d.ssrRenderer
	d.ssrRenderer = nil
	d.ssrMu.Unlock()
	if renderer != nil {
		if e := renderer.Close(); err == nil {
			err = e
		}
	}
	return err
}

// Graph 暴露模块图（id / version / imports / importers）。
// dev 包只存在于 -tags dev 构建，调用方无需担心生产二进制带图。
func (d *DevServer) Graph() *module.Graph { return d.graph }

// Module 是 Graph().Node 的便捷方法。
func (d *DevServer) Module(id string) (module.Node, bool) {
	if d.graph == nil {
		return module.Node{}, false
	}
	return d.graph.Node(id)
}

// Handler 返回可挂载的 http.Handler（框架无关；Gin 用 gin.WrapH 即可）。
//
// 配了 WithBasePath("/admin") 时，既可以直接 mux.Handle("/admin/", ds.Handler())，
// 也可以 http.StripPrefix("/admin", ds.Handler())——两种挂法都认。
func (d *DevServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.serveIndex)
	mux.HandleFunc("/src/", d.serveSrc)
	// 别名指向 src/ 之外（tsconfig paths、@shared/* 等）时，从项目根目录提供服务。
	// 只允许项目内的文件，避免 dev server 变成任意文件读取。
	mux.HandleFunc("/@fs/", d.serveFS)
	mux.HandleFunc("/@deps/", d.serveDeps)
	// 静态资源的原始字节：/src/logo.svg 返回的是 JS 模块（export default URL），
	// 真正给 <img> / CSS 用的是 /@wf/asset/<项目相对路径>。
	mux.HandleFunc("/@wf/asset/", d.serveAsset)
	// HMR runtime / 引导 / 覆盖层全部是外部脚本（无内联逻辑），便于 CSP。
	mux.HandleFunc("/@wf/client", d.serveClient)
	mux.HandleFunc("/@wf/boot", d.serveBoot)
	mux.HandleFunc("/@wf/overlay.js", d.serveOverlay)
	mux.HandleFunc("/__wf_hmr", d.serveWS)
	// 探活：Go 进程重启期间浏览器靠它判断"服务回来了没有"。
	// 必须足够轻——不要走 SSR，也不要有任何缓存。
	mux.HandleFunc("/__wf_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	// 最近一次编译错误：嵌套 import 失败时，浏览器只能拿到一句
	// "Failed to fetch dynamically imported module"，靠它换回真正的错误原文。
	mux.HandleFunc("/__wf_error", d.serveLastError)
	if d.basePath == "" {
		return mux
	}
	// 内部路由保持不带前缀：外层 mux 不会自动剥前缀，所以这里统一处理。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == d.basePath:
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			mux.ServeHTTP(&basePathLocationWriter{ResponseWriter: w, base: d.basePath}, r2)
		case strings.HasPrefix(p, d.basePath+"/"):
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, d.basePath)
			// ServeMux 的 clean-path / 补尾斜杠重定向发生在前缀剥离之后，
			// Location 会变成不带 basePath 的 /src/…；这里补回去。
			mux.ServeHTTP(&basePathLocationWriter{ResponseWriter: w, base: d.basePath}, r2)
		default:
			// 调用方已经 StripPrefix 过：路径本来就是内部形态。ServeMux 的
			// 重定向同样补前缀；writer 只在 Location 缺少前缀时改写，带前缀
			// 的 canonical / public 重定向不会被二次加前缀。
			mux.ServeHTTP(&basePathLocationWriter{ResponseWriter: w, base: d.basePath}, r)
		}
	})
}

// basePathLocationWriter 只修正 ServeMux 生成的重定向 Location，并透传
// Hijack/Flush，保证挂在前缀下的 HMR WebSocket 仍能升级。
type basePathLocationWriter struct {
	http.ResponseWriter
	base string
}

func (w *basePathLocationWriter) WriteHeader(code int) {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, "/") && loc != w.base && !strings.HasPrefix(loc, w.base+"/") {
			w.Header().Set("Location", w.base+loc)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *basePathLocationWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("dev: response writer 不支持 Hijack")
	}
	return h.Hijack()
}

func (w *basePathLocationWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *basePathLocationWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (d *DevServer) serveLastError(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	d.errMu.Lock()
	e := d.lastErr
	d.errMu.Unlock()
	if e == nil || e.ID <= since {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(e)
}

// servePublic 用 public 目录里的文件响应请求，命中返回 true。
// 未命中（未配置 / 不存在 / 不是普通文件）返回 false，由调用方继续走 SPA 外壳。
// 目录请求按 Vite 的惯例回目录下的 index.html。
func (d *DevServer) servePublic(w http.ResponseWriter, r *http.Request) bool {
	if d.publicDir == "" {
		return false
	}
	rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	full := filepath.Join(d.publicDir, filepath.FromSlash(rel))
	if _, ok := relWithin(d.publicDir, full); !ok {
		return false
	}
	fi, err := os.Stat(full)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		// 目录请求统一带尾斜杠：/docs → /docs/，否则 index.html 里的
		// 相对链接会以父目录为 base 解析（和文件系统语义不一致）。
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := d.url("/" + rel + "/")
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, target, http.StatusFound)
			return true
		}
		full = filepath.Join(full, "index.html")
		if fi, err = os.Stat(full); err != nil || fi.IsDir() {
			return false
		}
	}
	if !fi.Mode().IsRegular() {
		return false
	}
	// dev 态让浏览器每次回源校验；ServeFile 带 Last-Modified，文件没改就是 304。
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, full)
	return true
}

// serveIndex 是默认页面：public/ 静态文件优先，其余交给 RenderPage。
func (d *DevServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	// public/：内部路由（/src、/@deps、/@fs、/__wf_*）由 mux 的精确前缀先行命中，
	// 剩下的路径先看 <root>/public；根路径永远给 HMR 外壳，public/index.html
	// 不能把开发入口顶掉。
	if p := strings.TrimPrefix(path.Clean(r.URL.Path), "/"); p != "" && p != "index.html" {
		if d.servePublic(w, r) {
			return
		}
	}
	d.RenderPage(w, r, Page{})
}

// serveDeps 按需打包 npm 依赖包为 ESM 并缓存。
func (d *DevServer) serveDeps(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/@deps/")
	if raw == "" {
		http.Error(w, "missing dep specifier", http.StatusBadRequest)
		return
	}
	// /@deps 的 URL 是固定的（/@deps/react.js），不能依赖浏览器长缓存：
	// 依赖目录更新后客户端必须回源拿新包。
	w.Header().Set("Cache-Control", "no-cache")

	d.depsLock.RLock()
	cached, ok := d.depsCache[raw]
	d.depsLock.RUnlock()
	if ok {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(cached)
		return
	}

	pkgSpec := raw
	if strings.HasSuffix(pkgSpec, ".js") {
		trimmed := strings.TrimSuffix(pkgSpec, ".js")
		d.mu.Lock()
		_, isRoot := d.rootDeps[trimmed]
		d.mu.Unlock()
		if isRoot || strings.Contains(trimmed, "/") {
			pkgSpec = trimmed
		}
	}

	plugins := d.depPlugins(packageOf(pkgSpec))

	// CSS 子路径（如 nprogress/nprogress.css）：esbuild 的 CSS 产物不是 JS，
	// 浏览器不能直接 import。这里把它包成"导入即注入 <style>"的 ESM 模块。
	if strings.HasSuffix(pkgSpec, ".css") {
		code, errMsg := d.bundleDepCSS(pkgSpec, plugins)
		if errMsg != "" {
			d.fail(w, "/@deps/"+raw, errMsg)
			return
		}
		d.depsLock.Lock()
		d.depsCache[raw] = code
		d.depsLock.Unlock()
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(code)
		return
	}

	opts := api.BuildOptions{
		EntryPoints:   []string{pkgSpec},
		Bundle:        true,
		Format:        api.FormatESModule,
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: d.root,
		Plugins:       plugins,
		NodePaths:     d.nodePaths(),
		Define:        d.define,
		Write:         false,
	}
	res := api.Build(opts)
	if len(res.Errors) > 0 {
		d.fail(w, "/@deps/"+raw, "bundle dep "+pkgSpec+" error:\n"+compiler.FormatMessages(res.Errors))
		return
	}

	// CJS 对 CJS 的 require：esbuild 外置后留下的是浏览器跑不了的 __require()，
	// 先换成静态 import；再补入口的具名导出（`import { useState } from "react"`）。
	code := rewriteExternalRequires(res.OutputFiles[0].Contents, d.externals)
	code = addCJSInterop(code)
	d.depsLock.Lock()
	d.depsCache[raw] = code
	d.depsLock.Unlock()

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Write(code)
}

// depPlugins：外置插件必须排在 store 解析之前，否则 store 会先把裸导入
// 解析成文件，外置就轮不到了。
func (d *DevServer) depPlugins(entryPkg string) []api.Plugin {
	var plugins []api.Plugin
	if len(d.externals) > 0 {
		plugins = append(plugins, externalPlugin(d.externals, entryPkg))
	}
	return append(plugins, compiler.StorePlugins(d.root, d.modulesDir)...)
}

// nodePaths：依赖目录不叫 node_modules 且没有 store manifest 时，
// 仍要让 esbuild 能按 NODE_PATH 语义找到包。
func (d *DevServer) nodePaths() []string {
	if d.modulesDir == "" || d.modulesDir == "node_modules" {
		return nil
	}
	p := d.modulesDir
	if !filepath.IsAbs(p) {
		p = filepath.Join(d.root, p)
	}
	return []string{p}
}

// bundleDepCSS 打包依赖里的 CSS 子路径，返回注入样式的 ESM 模块。
// 返回值第二项非空即为错误文本（交给覆盖层展示）。
func (d *DevServer) bundleDepCSS(pkgSpec string, plugins []api.Plugin) ([]byte, string) {
	res := api.Build(api.BuildOptions{
		EntryPoints:   []string{pkgSpec},
		Bundle:        true,
		Write:         false,
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: d.root,
		Plugins:       plugins,
		NodePaths:     d.nodePaths(),
		Loader:        map[string]api.Loader{".css": api.LoaderCSS},
		Outdir:        "wf-dev-css",
		AssetNames:    "[name]-[hash]",
		Define:        d.define,
	})
	if len(res.Errors) > 0 {
		return nil, "bundle dep css " + pkgSpec + " error:\n" + compiler.FormatMessages(res.Errors)
	}
	var css strings.Builder
	for _, f := range res.OutputFiles {
		if strings.HasSuffix(f.Path, ".css") {
			css.Write(f.Contents)
		}
	}
	if css.Len() == 0 {
		return nil, "bundle dep css " + pkgSpec + ": no css output（检查包的 exports / 文件是否存在）"
	}
	return cssModuleJS(css.String(), "dep:"+pkgSpec, "", "", false), ""
}

// cssModuleJS 把 CSS 文本包成浏览器可 import 的模块：执行即注入/替换 <style>。
// data-wf-id 使用 canonical module ID，重复 import 同一模块时只更新内容，
// 不会在 <head> 里堆叠旧的 <style>。
//
// 源 CSS 同时是一个 HMR 边界：hmr=true 时注入 runtime + self-accept，
// 重新执行模块本身就会替换样式；依赖包里的 CSS（/@deps/...）不参与 HMR。
func cssModuleJS(css, id, ownerURL, clientURL string, hmr bool, depImports ...string) []byte {
	idJSON, _ := json.Marshal(id)
	cssJSON, _ := json.Marshal(css)
	prefix, suffix := "", ""
	if hmr && clientURL != "" {
		prefix = "import { createHotContext as __wfHotCreate } from " + strconv.Quote(clientURL) + ";\n" +
			"import.meta.hot = __wfHotCreate(" + strconv.Quote(ownerURL) + ");\n"
		suffix = "\nif (import.meta.hot) import.meta.hot.accept();"
	}
	// 顶层 @import 已由 transformCSS 变成真正的模块依赖：它们必须在本模块
	// 注入样式之前执行，CSS 级联顺序才和源文件一致。
	for _, dep := range depImports {
		prefix += "import " + strconv.Quote(dep) + ";\n"
	}
	return []byte(prefix + "var id=" + string(idJSON) +
		";var old=null;document.querySelectorAll('style[data-wf-id]').forEach(function(e){if(e.getAttribute('data-wf-id')===id)old=e;});" +
		"const s=document.createElement('style');s.setAttribute('data-wf-id',id);s.textContent=" + string(cssJSON) +
		";if(old&&old.parentNode){old.parentNode.replaceChild(s,old);}else{document.head.appendChild(s);}" +
		"export default s.textContent;" + suffix)
}

// aliasImport 把一个别名转成 import map 条目。
// 前缀别名（"@/"）保持尾斜杠；精确别名若是目录也补尾斜杠。
// 目标不在项目内时 ok=false —— dev server 不允许读出项目根目录。
func (d *DevServer) aliasImport(a compiler.Alias) (key, url string, ok bool) {
	u, ok := d.devURL(a.Target)
	if !ok {
		return "", "", false
	}
	if strings.HasSuffix(a.Prefix, "/") {
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		return a.Prefix, u, true
	}
	if fi, err := os.Stat(a.Target); err == nil && fi.IsDir() && !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return a.Prefix, u, true
}

// devURL 把项目内的绝对路径映射成 dev server 的 URL：
// src/ 下走 /src/（与入口一致），其余走 /@fs/（项目根相对）。
// 配了 basePath 时 URL 也带上挂载前缀（HMR 广播的 path 必须能直接 import）。
func (d *DevServer) devURL(p string) (string, bool) {
	abs := filepath.Clean(p)
	if rel, ok := relWithin(filepath.Join(d.root, "src"), abs); ok {
		if rel == "." {
			return d.url("/src/"), true
		}
		return d.url("/src/" + filepath.ToSlash(rel)), true
	}
	if rel, ok := relWithin(d.root, abs); ok {
		if rel == "." {
			return d.url("/@fs/"), true
		}
		return d.url("/@fs/" + filepath.ToSlash(rel)), true
	}
	return "", false
}

// relWithin 判断 p 是否在 base 内，返回相对路径（".", "..", 绝对路径都视为不在内）。
func relWithin(base, p string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// serveSrc 按需单文件转译 /src/ 下的文件。
func (d *DevServer) serveSrc(w http.ResponseWriter, r *http.Request) {
	d.serveURL(w, r, "/src/", filepath.Join(d.root, "src"))
}

// serveFS 服务别名指向的项目内文件（/@fs/<相对项目根的路径>）。
func (d *DevServer) serveFS(w http.ResponseWriter, r *http.Request) {
	d.serveURL(w, r, "/@fs/", d.root)
}

// serveURL 按需编译单个模块：读源码 → esbuild.Build（全部依赖外置）
// → 返回浏览器可 import 的产物，同时把 imports 记进模块图。
//
// 用 Build 而不是 Transform 的唯一目的就是拿到 OnResolve 回调：浏览器仍然
// 负责解析相对路径/import map，但服务端现在知道"这个模块依赖谁"，切片 1
// 的边界传播与说明符版本化都建立在它上面。
func (d *DevServer) serveURL(w http.ResponseWriter, r *http.Request, urlPrefix, baseDir string) {
	rel := strings.TrimPrefix(r.URL.Path, urlPrefix)
	// path.Clean("/"+rel) 把 ../ 收敛到根内，避免 filepath.Join 被逃逸出 baseDir。
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	full := filepath.Join(baseDir, filepath.FromSlash(rel))
	if _, ok := relWithin(baseDir, full); !ok {
		http.Error(w, "bad path: "+rel, http.StatusBadRequest)
		return
	}
	// 转译产物每次都回源校验；HMR 用 ?t=<seq> 穿透模块缓存，不依赖浏览器启发式缓存。
	w.Header().Set("Cache-Control", "no-cache")
	src, full, err := d.readSrc(full)
	if err != nil {
		// 文件不存在也要记一笔：子模块 404 在浏览器侧同样只会得到
		// "Failed to fetch dynamically imported module"，看不出是哪个文件。
		body := "模块不存在：" + r.URL.Path + "\n\nwf dev 服务 " +
			filepath.ToSlash(filepath.Join(d.root, "src")) + " 以及别名指向的项目内文件。"
		id, _ := module.CanonicalID(d.root, full)
		d.markFailed(id, true)
		d.recordError(r.URL.Path, body)
		http.Error(w, body, http.StatusNotFound)
		return
	}
	// 浏览器不做扩展名/index 补全：`import './router'` 请求 /src/router，
	// `import './lib/i18n'` 请求 /src/lib/i18n。这里已经把它解析到真实文件，
	// 但模块仍会以"请求 URL"为 base 解析内部的相对 import——目录会被当成
	// 文件，`./en` 就会跑到 /src/lib/en。先 302 到真实文件的 canonical URL，
	// 浏览器后续就以文件 URL 解析相对路径，HMR 的 owner URL 也才对得上。
	if d.redirectCanonical(w, r, full) {
		return
	}
	// Sourcefile 用项目相对路径（src/App.tsx），错误信息里的文件名才和构建期一致。
	rel = filepath.ToSlash(full)
	if r2, relErr := filepath.Rel(d.root, full); relErr == nil {
		rel = filepath.ToSlash(r2)
	}
	srcURL, _ := d.devURL(full)
	if srcURL == "" {
		srcURL = r.URL.Path
	}
	id, _ := module.CanonicalID(d.root, full)
	ext := strings.ToLower(filepath.Ext(full))
	switch ext {
	case ".tsx", ".ts", ".jsx", ".js", ".mjs", ".cjs":
		loader := api.LoaderTS
		switch ext {
		case ".tsx":
			loader = api.LoaderTSX
		case ".jsx":
			loader = api.LoaderJSX
		case ".js", ".mjs", ".cjs":
			loader = api.LoaderJS
		}
		code, imports, err := d.buildModule(full, rel, string(src), srcURL, loader)
		if err != nil {
			d.failModule(w, srcURL, err.Error(), id)
			return
		}
		d.recordModule(full, module.KindJS, code, imports, hmrAcceptPattern.Match(src), "")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(code)
	case ".css":
		code, imports, err := d.buildModule(full, rel, string(src), srcURL, api.LoaderCSS)
		if err != nil {
			d.failModule(w, srcURL, err.Error(), id)
			return
		}
		cssText, cssImports, imports := d.transformCSS(string(code), full, imports)
		out := cssModuleJS(cssText, id, srcURL, d.url("/@wf/client"), true, cssImports...)
		d.recordModule(full, module.KindCSS, out, imports, true, "css")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(out)
	case ".vue":
		// dev 专有的 HMR 产物：__hmrId/__file + import.meta.hot.accept；
		// 生产构建走 CompileSFCResult 的默认分支，产物里没有这些符号。
		// PreviousScriptHash 用来区分"只改模板"（rerender，保留状态）与
		// "脚本也改了"（reload）。
		d.sfcMu.Lock()
		prevScript := d.sfcScript[module.Key(id)]
		d.sfcMu.Unlock()
		res, err := compiler.CompileSFCResultWithOptions(d.root, d.modulesDir, string(src), full, compiler.SFCOptions{
			HMR:                true,
			PreviousScriptHash: prevScript,
		})
		if err != nil {
			// err 自带文件名与阶段（parse / compileScript / compileTemplate / <style>），
			// 不再加前缀，覆盖层会把首行当标题。
			d.failModule(w, srcURL, err.Error(), id)
			return
		}
		// 与构建期一致：lang="ts" 的产物要再过一遍 TS 转译，否则浏览器会在
		// `interface` / `ref<number>()` 处直接语法报错。SFC 产物里的 import
		// 仍然相对 .vue 文件，ResolveDir 用文件所在目录即可。
		code, imports, err := d.buildModule(full, rel, res.Code, srcURL, compiler.SFCLoader(res.Lang))
		if err != nil {
			d.failModule(w, srcURL, "vue ts transform error:\n"+err.Error(), id)
			return
		}
		d.sfcMu.Lock()
		if d.sfcScript == nil {
			d.sfcScript = map[string]string{}
		}
		d.sfcScript[module.Key(id)] = res.ScriptHash
		d.sfcMu.Unlock()
		// self-accept / kind 走注册表：默认 Vue 适配器给出 true / "vue"，
		// WithAdapters 覆盖 .vue 时也会跟着变。
		d.recordModule(full, module.KindVue, code, imports, d.adapters.SelfAccepts(id), d.adapters.HMRKind(id))
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(code)
	case ".json":
		code, imports, err := d.buildModule(full, rel, string(src), srcURL, api.LoaderJSON)
		if err != nil {
			d.failModule(w, srcURL, err.Error(), id)
			return
		}
		d.recordModule(full, module.KindJSON, code, imports, false, "json")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(code)
	default:
		if isAssetExt(ext) {
			d.serveAssetModule(w, r, full, src)
			return
		}
		http.Error(w, "webfront dev: unsupported source type "+ext, http.StatusBadRequest)
	}
}

// buildModule 用 esbuild.Build 转译单个模块，并通过 OnResolve 收集依赖边。
//
// 所有 import 都标记为 External：浏览器仍按说明符请求（import map 负责 npm
// 包）。依赖被标记为 dirty 时，OnResolve 返回带 ?t=<seq> 的改写路径，这就是
// C3：边界重新执行时，链路上每个确实变过的本地依赖都会拿到新 URL。
//
// ownerURL 是模块的 canonical dev URL（含 basePath），banner 用它登记 HMR
// 记录；banner 是输出期拼接，不会成为依赖边。
func (d *DevServer) buildModule(full, sourcefile, contents, ownerURL string, loader api.Loader) ([]byte, []module.Import, error) {
	var mu sync.Mutex
	var imports []module.Import
	seen := map[string]struct{}{}
	importerDir := filepath.Dir(full)
	dirty := d.dirtySnapshot()
	// 被删除的依赖已经解析不到了，但旧图里还留着"哪个说明符指向哪个
	// canonical ID"。用它给说明符补 ?t：否则边界重新执行时会请求原来
	// 的裸 URL，浏览器直接命中旧模块缓存，删除永远无法暴露成 import 错误。
	prevTargets := map[string]string{}
	if importerID, ok := module.CanonicalID(d.root, full); ok {
		if node, ok := d.graph.Node(importerID); ok {
			for _, prev := range node.Imports {
				if prev.Resolved != "" && !prev.External {
					prevTargets[prev.Specifier] = prev.Resolved
				}
			}
		}
	}
	plugin := api.Plugin{
		Name: "webfront-dev-graph",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				imp := d.resolveImport(importerDir, args.Path, args.Kind)
				key := imp.Specifier + "\x00" + imp.Resolved + "\x00" + imp.Kind
				mu.Lock()
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					imports = append(imports, imp)
				}
				mu.Unlock()
				out := api.OnResolveResult{Path: args.Path, External: true}
				target := imp.Resolved
				if target == "" && !imp.External {
					target = prevTargets[args.Path]
				}
				if target != "" {
					if v := dirty[module.Key(target)]; v > 0 {
						versioned := imp
						versioned.Resolved = target
						out.Path = d.versionedSpecifier(args.Path, versioned, v)
					}
				}
				return out, nil
			})
		},
	}
	res := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   contents,
			ResolveDir: importerDir,
			Sourcefile: sourcefile,
			Loader:     loader,
		},
		Bundle:        true,
		Write:         false,
		Format:        api.FormatESModule,
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: d.root,
		Plugins:       []api.Plugin{plugin},
		Banner:        map[string]string{"js": d.hotBanner(ownerURL)},
		Define:        d.define,
		TsconfigRaw:   d.tsconfigRaw,
		Sourcemap:     api.SourceMapInline,
		Metafile:      true,
		Target:        api.ESNext,
		LogLevel:      api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		return nil, nil, fmt.Errorf("%s", compiler.FormatMessages(res.Errors))
	}
	if len(res.OutputFiles) == 0 {
		return nil, nil, fmt.Errorf("esbuild: no output for %s", sourcefile)
	}
	sort.Slice(imports, func(i, j int) bool {
		if imports[i].Specifier != imports[j].Specifier {
			return imports[i].Specifier < imports[j].Specifier
		}
		return imports[i].Kind < imports[j].Kind
	})
	return res.OutputFiles[0].Contents, imports, nil
}

// resolveImport 把一条 import 说明符解析成 canonical module ID。
// 裸包/URL/data: 交给浏览器（import map / 原生 URL），本地文件解析到图节点。
func (d *DevServer) resolveImport(importerDir, spec string, kind api.ResolveKind) module.Import {
	imp := module.Import{Specifier: spec, Kind: resolveKindName(kind)}
	raw := spec
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" || strings.Contains(raw, "://") || strings.HasPrefix(raw, "data:") ||
		strings.HasPrefix(raw, "node:") || strings.HasPrefix(raw, "#") {
		imp.External = true
		return imp
	}

	var cand string
	switch {
	case strings.HasPrefix(raw, "."):
		cand = filepath.Join(importerDir, filepath.FromSlash(raw))
	case strings.HasPrefix(raw, "/@fs/"):
		cand = filepath.Join(d.root, filepath.FromSlash(strings.TrimPrefix(raw, "/@fs/")))
	case strings.HasPrefix(raw, "/src/"):
		cand = filepath.Join(d.root, "src", filepath.FromSlash(strings.TrimPrefix(raw, "/src/")))
	case strings.HasPrefix(raw, "/"):
		cand = filepath.Join(d.root, filepath.FromSlash(strings.TrimPrefix(raw, "/")))
	case filepath.IsAbs(raw):
		cand = raw
	default:
		target, ok := compiler.ResolveAlias(d.aliases, raw)
		if !ok {
			// 裸包（含子路径）由 import map 路由到 /@deps，图里记为 external。
			imp.External = true
			return imp
		}
		cand = target
	}

	resolved := compiler.ResolveSourceFile(cand)
	if resolved == "" {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			resolved = cand
		}
	}
	if resolved == "" {
		// 本地文件但暂时解析不到：保留边，Resolved 为空，importer 不会因此断开。
		return imp
	}
	id, ok := module.CanonicalID(d.root, resolved)
	if !ok {
		// 项目外的文件 dev server 不提供，按 external 处理（构建期仍会打包）。
		imp.External = true
		return imp
	}
	imp.Resolved = id
	imp.Asset = isAssetExt(filepath.Ext(resolved))
	return imp
}

func resolveKindName(k api.ResolveKind) string {
	switch k {
	case api.ResolveEntryPoint:
		return "entry-point"
	case api.ResolveJSImportStatement:
		return "import-statement"
	case api.ResolveJSRequireCall:
		return "require-call"
	case api.ResolveJSDynamicImport:
		return "dynamic-import"
	case api.ResolveJSRequireResolve:
		return "require-resolve"
	case api.ResolveCSSImportRule:
		return "import-rule"
	case api.ResolveCSSComposesFrom:
		return "composes-from"
	case api.ResolveCSSURLToken:
		return "url-token"
	}
	return "unknown"
}

// recordModule 更新模块图。Hash 稳定时 version 不变，重复请求不会让
// HMR 客户端误以为模块变了。selfAccept/hmrKind 是 HMR 边界与 wire kind。
func (d *DevServer) recordModule(full string, kind module.Kind, code []byte, imports []module.Import, selfAccept bool, hmrKind string) {
	if d.graph == nil {
		return
	}
	id, ok := module.CanonicalID(d.root, full)
	if !ok {
		return
	}
	url, ok := d.devURL(full)
	if !ok {
		return
	}
	sum := sha256.Sum256(code)
	d.graph.Set(module.Node{
		ID:         id,
		URL:        url,
		Kind:       kind,
		Hash:       hex.EncodeToString(sum[:]),
		SelfAccept: selfAccept,
		HMRKind:    hmrKind,
		Imports:    imports,
	})
	// 成功编译（哪怕内容没变）就清掉失败标记；修好后的下一次变更走正常边界。
	d.markFailed(id, false)
}

// isAssetExt 与 build 的静态资源加载器共用 compiler 的同一张扩展名表。
func isAssetExt(ext string) bool { return compiler.IsAssetExt(ext) }

// serveAssetModule 把静态资源包成 JS 模块：import logo from './logo.svg'
// 得到默认导出 /@wf/asset/src/logo.svg；?raw 返回文件文本，?url 与默认一致。
func (d *DevServer) serveAssetModule(w http.ResponseWriter, r *http.Request, full string, src []byte) {
	id, ok := module.CanonicalID(d.root, full)
	if !ok {
		http.Error(w, "asset outside project", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	var code []byte
	if _, raw := q["raw"]; raw {
		text, _ := json.Marshal(string(src))
		code = []byte("export default " + string(text) + ";")
	} else {
		u, _ := json.Marshal(d.url("/@wf/asset/" + id))
		code = []byte("export default " + string(u) + ";")
	}
	d.recordModule(full, module.KindAsset, code, nil, false, "asset")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(code)
}

// serveAsset 直出 /@wf/asset/<项目相对路径> 的原始字节（图片、字体等）。
func (d *DevServer) serveAsset(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/@wf/asset/")
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	full := filepath.Join(d.root, filepath.FromSlash(rel))
	if _, ok := relWithin(d.root, full); !ok {
		http.Error(w, "bad asset path: "+rel, http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, full)
}

// canonicalPath 去掉挂载前缀，只比较 dev server 内部路径。
// 请求可能已经过 basePath wrapper 或 http.StripPrefix，两种形态都认。
func (d *DevServer) canonicalPath(p string) string {
	if d.basePath == "" {
		return p
	}
	if p == d.basePath {
		return "/"
	}
	if strings.HasPrefix(p, d.basePath+"/") {
		return strings.TrimPrefix(p, d.basePath)
	}
	return p
}

// redirectCanonical 把"扩展名/index 补全后的请求 URL"重定向到真实文件 URL。
// 必须在 readSrc 解析成功之后调用；查询串（?t / ?raw）原样带过去，
// 302 而不是 301：文件解析结果可能随目录内容变化，不能被浏览器长期缓存。
func (d *DevServer) redirectCanonical(w http.ResponseWriter, r *http.Request, full string) bool {
	canonical, ok := d.devURL(full)
	if !ok || canonical == "" {
		return false
	}
	// 必须按原样比较：path.Clean 会把 /src/router.ts/ 当成 /src/router.ts，
	// 但浏览器解析相对 import 时尾斜杠会改变 base，模块会请求到
	// /src/router.ts/pages/x 这种 404 URL。
	if d.canonicalPath(canonical) == d.canonicalPath(r.URL.Path) {
		return false
	}
	target := canonical
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

// readSrc 读源码文件，并按 Vite 的规则补全路径。
//
// 浏览器不做扩展名补全：`import App from './App'` 会原样请求 /src/App。
// 构建期由 esbuild 解析，dev 期没人代劳，所以这里补：
// 原路径 → 路径+扩展名 → 路径/index+扩展名（顺序与 build 共用）。
func (d *DevServer) readSrc(full string) ([]byte, string, error) {
	p := compiler.ResolveSourceFile(full)
	if p == "" {
		return nil, full, os.ErrNotExist
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, p, err
	}
	return b, p, nil
}

func (d *DevServer) serveWS(w http.ResponseWriter, r *http.Request) {
	if d.isClosed() {
		http.Error(w, "dev server closed", http.StatusServiceUnavailable)
		return
	}
	conn, err := d.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsClient{
		conn: conn,
		send: make(chan []byte, 32),
		done: make(chan struct{}),
	}

	// connected 必须先入队、再注册客户端、最后启动 writer。若先注册再入队，
	// 并发的 flush 可能把 batch 塞进 channel，客户端收到的第一条消息就不是
	// connected，协议校验也就漏掉了。
	if hello, err := json.Marshal(d.connectedMessage()); err == nil {
		select {
		case c.send <- hello:
		default:
		}
	}

	d.mu.Lock()
	if d.isClosed() {
		d.mu.Unlock()
		_ = conn.Close()
		return
	}
	if d.clients == nil {
		d.clients = map[*wsClient]struct{}{}
	}
	d.clients[c] = struct{}{}
	// wg.Add 与注册在同一临界区：Close 先 close(d.closed) 再拿 d.mu 遍历
	// 客户端，因此要么它看见这个客户端并关掉，要么我们看到 closed 直接返回；
	// 不会出现 Wait 之后才 Add 的竞态。
	d.wg.Add(1)
	d.mu.Unlock()

	// writer：广播/心跳都走这里，HTTP handler 只负责读，避免两个 goroutine
	// 同时 WriteMessage（gorilla/websocket 不支持并发写）。
	go func() {
		defer d.wg.Done()
		c.writeLoop(d)
	}()

	// reader 留在当前 handler 里：连接断开时立即清理。read limit 覆盖
	// hot.send() 的自定义消息（默认 1KB 会把正常 JSON 直接断连）。
	conn.SetReadLimit(64 * 1024)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	defer d.removeClient(c)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	}
}
