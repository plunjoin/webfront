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

func NewDevServer(root string) (*DevServer, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(root)

	rootDeps := map[string]string{}
	modulesDir := "node_modules"

	// 1. 优先从版本化 store 的 manifest.json 中读取 root 依赖
	for _, md := range []string{"wf_modules", "node_modules"} {
		sd := filepath.Join(abs, md, ".store")
		if m, err := compiler.LoadStoreManifest(sd); err == nil {
			modulesDir = md
			for k, v := range m.Root {
				rootDeps[k] = v
			}
			break
		}
	}

	// 2. 补充读取 package.json dependencies
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

	d := &DevServer{
		root:       abs,
		entry:      detectDevEntry(abs),
		watcher:    w,
		clients:    map[*websocket.Conn]bool{},
		depsCache:  map[string][]byte{},
		rootDeps:   rootDeps,
		modulesDir: modulesDir,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
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
		if ext == ".tsx" || ext == ".ts" || ext == ".vue" || ext == ".css" || ext == ".js" || ext == ".jsx" {
			rel, _ := filepath.Rel(d.root, ev.Name)
			rel = filepath.ToSlash(rel)
			d.broadcast(map[string]any{"type": "update", "path": "/" + rel})
			fmt.Printf("[HMR] changed %s -> broadcast to %d client(s)\n", rel, d.clientCount())
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
func (d *DevServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.serveIndex)
	mux.HandleFunc("/src/", d.serveSrc)
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
	return mux
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
		imports[pkg] = "/@deps/" + pkg + ".js"
		imports[pkg+"/"] = "/@deps/" + pkg + "/"
	}
	d.mu.Unlock()

	importMapJSON, _ := json.MarshalIndent(importMap, "  ", "  ")
	importMapTag := ""
	if len(imports) > 0 {
		importMapTag = "\n  <script type=\"importmap\">\n  " + string(importMapJSON) + "\n  </script>\n"
	}

	// 用字符串拼接而不是 Sprintf：客户端脚本里将来出现 % 也不会被当成格式动词。
	html := "<!doctype html><html><head><meta charset=\"utf-8\"><title>webfront dev (HMR)</title>" +
		importMapTag +
		"</head>\n<body>\n<div id=\"root\"></div>\n<script>\n" +
		clientScript(d.entry) +
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

	plugins := compiler.StorePlugins(d.root, d.modulesDir)
	opts := api.BuildOptions{
		EntryPoints:   []string{pkgSpec},
		Bundle:        true,
		Format:        api.FormatESModule,
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: d.root,
		Plugins:       plugins,
		Define:        map[string]string{"process.env.NODE_ENV": "\"development\""},
		Write:         false,
	}
	res := api.Build(opts)
	if len(res.Errors) > 0 {
		d.fail(w, "/@deps/"+raw, "bundle dep "+pkgSpec+" error:\n"+compiler.FormatMessages(res.Errors))
		return
	}

	code := res.OutputFiles[0].Contents
	d.depsLock.Lock()
	d.depsCache[raw] = code
	d.depsLock.Unlock()

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Write(code)
}

// serveSrc 按需单文件转译：读源码 → esbuild.Transform / Vue CompileSFC → 返回浏览器可 import 的产物。
//   - .ts/.tsx/.js/.jsx：转译为标准 ESM JS
//   - .css：用 esbuild 转成"导入即注入 <style>"的 ESM 模块（支持 @import / url()）
//   - .vue：通过 compiler.CompileSFC 编译单文件组件（包含模板、脚本与样式注入）
func (d *DevServer) serveSrc(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/src/")
	// path.Clean("/"+rel) 把 ../ 收敛到根内，避免 filepath.Join 被逃逸出 src/。
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	full := filepath.Join(d.root, "src", filepath.FromSlash(rel))
	if !strings.HasPrefix(full, filepath.Join(d.root, "src")+string(filepath.Separator)) {
		http.Error(w, "bad path: "+rel, http.StatusBadRequest)
		return
	}
	src, full, err := d.readSrc(full)
	if err != nil {
		// 文件不存在也要记一笔：子模块 404 在浏览器侧同样只会得到
		// "Failed to fetch dynamically imported module"，看不出是哪个文件。
		body := "模块不存在：" + r.URL.Path + "\n\nwf dev 只服务 " +
			filepath.ToSlash(filepath.Join(d.root, "src")) + " 下的文件。"
		d.recordError(r.URL.Path, body)
		http.Error(w, body, http.StatusNotFound)
		return
	}
	// 补全后的真实路径才是 Sourcefile 该用的名字：浏览器请求 /src/App，
	// 但错误里必须写 App.tsx，否则用户按行号找不到文件。
	if r2, relErr := filepath.Rel(filepath.Join(d.root, "src"), full); relErr == nil {
		rel = filepath.ToSlash(r2)
	}
	srcURL := "/src/" + rel
	ext := filepath.Ext(full)
	switch ext {
	case ".tsx", ".ts", ".jsx", ".js":
		loader := api.LoaderTS
		switch ext {
		case ".tsx":
			loader = api.LoaderTSX
		case ".jsx":
			loader = api.LoaderJSX
		case ".js":
			loader = api.LoaderJS
		}
		res := api.Transform(string(src), api.TransformOptions{
			Loader:     loader,
			Format:     api.FormatESModule,
			Sourcefile: rel,
			Define:     map[string]string{"process.env.NODE_ENV": "\"development\""},
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
		cssJSON, _ := json.Marshal(string(res.Code))
		js := "const s=document.createElement('style');s.textContent=" + string(cssJSON) +
			";document.head.appendChild(s);export default s.textContent;"
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write([]byte(js))
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
			Loader:     compiler.SFCLoader(res.Lang),
			Format:     api.FormatESModule,
			Sourcefile: rel,
			Define:     map[string]string{"process.env.NODE_ENV": "\"development\""},
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

// srcExts 是"无扩展名导入"的补全顺序，与 esbuild 的 resolveExtensions 一致。
var srcExts = []string{".tsx", ".ts", ".jsx", ".js", ".vue", ".css", ".json"}

// readSrc 读源码文件，并按 Vite 的规则补全路径。
//
// 浏览器不做扩展名补全：`import App from './App'` 会原样请求 /src/App。
// 构建期由 esbuild 解析，dev 期没人代劳，所以这里补：
// 原路径 → 路径+扩展名 → 路径/index+扩展名。
func (d *DevServer) readSrc(full string) ([]byte, string, error) {
	if b, err := os.ReadFile(full); err == nil {
		return b, full, nil
	}
	if filepath.Ext(full) == "" {
		for _, ext := range srcExts {
			p := full + ext
			if b, err := os.ReadFile(p); err == nil {
				return b, p, nil
			}
		}
	}
	for _, ext := range srcExts {
		p := filepath.Join(full, "index"+ext)
		if b, err := os.ReadFile(p); err == nil {
			return b, p, nil
		}
	}
	return nil, full, os.ErrNotExist
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
