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
	case "node_modules", "wf_modules", "dist", ".git", ".store":
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
	return mux
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

	entry := strconv.Quote(d.entry)
	html := fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><title>webfront dev (HMR)</title>`+
		importMapTag+
		`</head>
<body>
<div id="root"></div>
<script>
const ws = new WebSocket('ws://' + location.host + '/__wf_hmr');
let render = null;
ws.onmessage = async (e) => {
  const m = JSON.parse(e.data);
  if (m.type === 'update') {
    try {
      const isCSS = m.path.endsWith('.css');
      const mod = await import(m.path + '?t=' + Date.now());
      if (mod.render) {
        render = mod.render;
        render();
      } else if (isCSS) {
        // .css 模块已动态更新注入 DOM，无需刷新页面
      } else {
        // 普通子组件（如 App.tsx、App.vue 等）：触发轻量刷新以穿透浏览器 ESM 缓存
        location.reload();
      }
    } catch (err) {
      console.error('[HMR] update failed, reloading page...', err);
      location.reload();
    }
  }
};
import(%s + '?t=' + Date.now()).then(m => {
  // main.* 约定导出 render() 以便热替换；entry-client.* 自己挂载，没有 render 也正常。
  if (m && typeof m.render === 'function') { render = m.render; render(); }
}).catch(e => console.error('[HMR] initial import failed', e));
</script>
</body></html>`, entry)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
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
		http.Error(w, fmt.Sprintf("bundle dep %s error: %+v", pkgSpec, res.Errors), http.StatusInternalServerError)
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
	src, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, "not found: "+rel, 404)
		return
	}
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
			http.Error(w, fmt.Sprintf("%+v", res.Errors), 500)
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
			http.Error(w, fmt.Sprintf("%+v", res.Errors), 500)
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
			http.Error(w, fmt.Sprintf("vue compile error: %v", err), http.StatusInternalServerError)
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
			http.Error(w, fmt.Sprintf("vue ts transform error: %+v", tr.Errors), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(tr.Code)
	default:
		http.Error(w, "webfront dev: unsupported source type "+ext, http.StatusBadRequest)
	}
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
