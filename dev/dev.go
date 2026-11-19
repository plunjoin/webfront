package dev

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"

	"github.com/plunjoin/webfront/compiler"
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
	clients    map[*websocket.Conn]bool
	depsLock   sync.RWMutex
	depsCache  map[string][]byte
	rootDeps   map[string]string
	modulesDir string

	// 挂载前缀（"" 表示根路径）：生成的 URL、import map、HMR 地址都带上它。
	basePath string

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
}

// compileError 是一条编译错误的快照，JSON 直接喂给浏览器。
type compileError struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Body string `json:"body"`
	At   string `json:"at"`
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
		clients:   map[*websocket.Conn]bool{},
		depsCache: map[string][]byte{},
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	for _, opt := range opts {
		opt(d)
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
	// fsnotify 不递归：必须把 src 下每一层子目录都加进监听，
	// 否则改 src/components/Foo.vue 这种文件时 HMR 完全收不到事件。
	d.watchDirs(abs)
	fmt.Printf("[HMR] watching %s (recursive)\n", abs)
	go d.watch()
	return d, nil
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
func (d *DevServer) watch() {
	for ev := range d.watcher.Events {
		// 只关心"写入/新建"。改名式保存（写 .tmp 再 rename 覆盖）在 Windows 上会先发
		// Remove(旧文件) 再 Create(新文件)：若在 Remove 时就广播，浏览器 import 会撞上
		// 文件短暂不存在的 404。故跳过 Remove/Rename/Chmod，只在新内容就绪时热更。
		if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
			continue
		}
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			// 新建目录（如 src/components）也要纳入监听，否则后续改动收不到。
			d.watchDirs(ev.Name)
			continue
		}
		ext := filepath.Ext(ev.Name)
		if ext == ".tsx" || ext == ".ts" || ext == ".vue" || ext == ".css" || ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs" {
			// 用 devURL 而不是拼 "/"+相对路径：src/ 下的文件映射到 /src/，
			// 别名指向的其他目录（shared/ 等）映射到 /@fs/，否则浏览器 import 会撞上 index.html。
			url, ok := d.devURL(ev.Name)
			if !ok {
				continue
			}
			rel, _ := filepath.Rel(d.root, ev.Name)
			d.broadcast(map[string]any{"type": "update", "path": url})
			fmt.Printf("[HMR] changed %s -> broadcast to %d client(s)\n", filepath.ToSlash(rel), d.clientCount())
		}
	}
}

func (d *DevServer) clientCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.clients)
}

func (d *DevServer) broadcast(msg any) {
	b, _ := json.Marshal(msg)
	d.mu.Lock()
	defer d.mu.Unlock()
	for c := range d.clients {
		_ = c.WriteMessage(websocket.TextMessage, b)
	}
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
			mux.ServeHTTP(w, r2)
		case strings.HasPrefix(p, d.basePath+"/"):
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, d.basePath)
			mux.ServeHTTP(w, r2)
		default:
			// 调用方已经 StripPrefix 过：路径本来就是内部形态，直接交给 mux。
			mux.ServeHTTP(w, r)
		}
	})
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

// serveIndex 注入 HMR 客户端脚本 + ImportMap + 模块入口。
func (d *DevServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	importMap := map[string]any{
		"imports": map[string]string{},
	}
	imports := importMap["imports"].(map[string]string)
	d.mu.Lock()
	for pkg := range d.rootDeps {
		imports[pkg] = d.url("/@deps/" + pkg + ".js")
		imports[pkg+"/"] = d.url("/@deps/" + pkg + "/")
	}
	// 显式 WithExternal 指定的包即使不在 rootDeps 里也要有映射，
	// 否则浏览器无法解析外置后的裸导入。
	for pkg := range d.externals {
		if _, ok := imports[pkg]; ok {
			continue
		}
		imports[pkg] = d.url("/@deps/" + pkg + ".js")
		imports[pkg+"/"] = d.url("/@deps/" + pkg + "/")
	}
	d.mu.Unlock()

	// 别名放在依赖之后：同名时用户显式配置的别名优先于 npm 包。
	for _, a := range d.aliases {
		if key, val, ok := d.aliasImport(a); ok {
			imports[key] = val
		}
	}

	importMapJSON, _ := json.MarshalIndent(importMap, "  ", "  ")
	importMapTag := ""
	if len(imports) > 0 {
		importMapTag = "\n  <script type=\"importmap\">\n  " + string(importMapJSON) + "\n  </script>\n"
	}

	// 用字符串拼接而不是 Sprintf：客户端脚本里将来出现 % 也不会被当成格式动词。
	html := "<!doctype html><html><head><meta charset=\"utf-8\"><title>webfront dev (HMR)</title>" +
		importMapTag +
		"</head>\n<body>\n<div id=\"root\"></div>\n<script>\n" +
		clientScript(d.url(d.entry), d.basePath) +
		"</script>\n</body></html>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

// serveDeps 按需打包 npm 依赖包为 ESM 并缓存。
func (d *DevServer) serveDeps(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/@deps/")
	if raw == "" {
		http.Error(w, "missing dep specifier", http.StatusBadRequest)
		return
	}

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
	return cssModuleJS(css.String()), ""
}

// cssModuleJS 把 CSS 文本包成浏览器可 import 的模块：执行即注入 <style>。
func cssModuleJS(css string) []byte {
	cssJSON, _ := json.Marshal(css)
	return []byte("const s=document.createElement('style');s.textContent=" + string(cssJSON) +
		";document.head.appendChild(s);export default s.textContent;")
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

// serveURL 按需单文件转译：读源码 → esbuild.Transform / Vue CompileSFC → 返回浏览器可 import 的产物。
//   - .ts/.tsx/.js/.jsx：转译为标准 ESM JS
//   - .css：用 esbuild 转成"导入即注入 <style>"的 ESM 模块（支持 @import / url()）
//   - .vue：通过 compiler.CompileSFC 编译单文件组件（包含模板、脚本与样式注入）
func (d *DevServer) serveURL(w http.ResponseWriter, r *http.Request, urlPrefix, baseDir string) {
	rel := strings.TrimPrefix(r.URL.Path, urlPrefix)
	// path.Clean("/"+rel) 把 ../ 收敛到根内，避免 filepath.Join 被逃逸出 baseDir。
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	full := filepath.Join(baseDir, filepath.FromSlash(rel))
	if _, ok := relWithin(baseDir, full); !ok {
		http.Error(w, "bad path: "+rel, http.StatusBadRequest)
		return
	}
	src, full, err := d.readSrc(full)
	if err != nil {
		// 文件不存在也要记一笔：子模块 404 在浏览器侧同样只会得到
		// "Failed to fetch dynamically imported module"，看不出是哪个文件。
		body := "模块不存在：" + r.URL.Path + "\n\nwf dev 服务 " +
			filepath.ToSlash(filepath.Join(d.root, "src")) + " 以及别名指向的项目内文件。"
		d.recordError(r.URL.Path, body)
		http.Error(w, body, http.StatusNotFound)
		return
	}
	// 补全后的真实路径才是 Sourcefile 该用的名字：浏览器请求 /src/App，
	// 但错误里必须写 App.tsx，否则用户按行号找不到文件。
	if r2, relErr := filepath.Rel(baseDir, full); relErr == nil {
		rel = filepath.ToSlash(r2)
	}
	srcURL := urlPrefix + rel
	ext := filepath.Ext(full)
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
		res := api.Transform(string(src), api.TransformOptions{
			Loader:      loader,
			Format:      api.FormatESModule,
			Sourcefile:  rel,
			TsconfigRaw: d.tsconfigRaw,
			Define:      d.define,
		})
		if len(res.Errors) > 0 {
			d.fail(w, srcURL, compiler.FormatMessages(res.Errors))
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(res.Code)
	case ".css":
		res := api.Transform(string(src), api.TransformOptions{
			Loader:     api.LoaderCSS,
			Format:     api.FormatESModule,
			Sourcefile: rel,
		})
		if len(res.Errors) > 0 {
			d.fail(w, srcURL, compiler.FormatMessages(res.Errors))
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(cssModuleJS(string(res.Code)))
	case ".vue":
		res, err := compiler.CompileSFCResult(d.root, d.modulesDir, string(src), full)
		if err != nil {
			// err 自带文件名与阶段（parse / compileScript / compileTemplate / <style>），
			// 不再加前缀，覆盖层会把首行当标题。
			d.fail(w, srcURL, err.Error())
			return
		}
		// 与构建期一致：lang="ts" 的产物要再过一遍 TS 转译，否则浏览器会在
		// `interface` / `ref<number>()` 处直接语法报错。
		tr := api.Transform(res.Code, api.TransformOptions{
			Loader:      compiler.SFCLoader(res.Lang),
			Format:      api.FormatESModule,
			Sourcefile:  rel,
			TsconfigRaw: d.tsconfigRaw,
			Define:      d.define,
		})
		if len(tr.Errors) > 0 {
			d.fail(w, srcURL, "vue ts transform error:\n"+compiler.FormatMessages(tr.Errors))
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(tr.Code)
	case ".json":
		res := api.Transform(string(src), api.TransformOptions{
			Loader:     api.LoaderJSON,
			Format:     api.FormatESModule,
			Sourcefile: rel,
		})
		if len(res.Errors) > 0 {
			d.fail(w, srcURL, compiler.FormatMessages(res.Errors))
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(res.Code)
	default:
		http.Error(w, "webfront dev: unsupported source type "+ext, http.StatusBadRequest)
	}
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
	conn, err := d.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	d.mu.Lock()
	d.clients[conn] = true
	d.mu.Unlock()
	// 保持连接，断开时清理
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			d.mu.Lock()
			delete(d.clients, conn)
			d.mu.Unlock()
			conn.Close()
			return
		}
	}
}
