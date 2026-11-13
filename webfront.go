// Package webfront: a 100% pure-Go front-end runtime.
// No Node.js, no CGO. Embeds as an http.Handler / Gin middleware.
//
// 三种运行形态，链接成本差一个数量级：
//   - 动态 SSR：Config.Renderer = jsruntime.New(ssrBundle, n) → 带 JS 引擎（≈15.6MB）
//   - 预渲染/静态：不设 Renderer，dist 里有 pages/*.html → 直接发 HTML（≈6.9MB，无引擎）
//   - 纯客户端 SPA：不设 Renderer，dist 里只有 index.html → 文件 + History fallback（≈6.9MB）
package webfront

import (
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/plunjoin/webfront/devui"
)

type Mode string

const (
	// ModeSPA：ServeHTTP 只做文件服务 + index.html History fallback，不跑 JS。
	ModeSPA Mode = "spa"
	// ModeSSR：Renderer 非 nil 时动态 SSR；nil 时服务预渲染页（静态 / SSG）。
	ModeSSR Mode = "ssr"
	// ModeIslands 仍是占位值：ServeHTTP 退化为 SSR 分支。
	ModeIslands Mode = "islands"
)

// Config：遵循"零配置可用，按需覆盖"
type Config struct {
	RootDir string // 前端工作目录，默认 "./web"
	// BasePath 路由挂载前缀，默认 "/"（即挂根）。
	// 设为 "/site" 时：注入的客户端脚本路径自动带前缀；预渲染页面查找、
	// StaticHandler 取文件都会先剥掉这个前缀。前端代码里仍按 "/about" 写路由，
	// 不需要知道自己被挂在哪儿。
	BasePath string
	// Mode 可选。零值表示自动识别：
	//   Renderer != nil           → ModeSSR（动态 SSR）
	//   dist 里有 pages/**.html   → ModeSSR（预渲染 / 静态）
	//   dist 里有 index.html      → ModeSPA（纯客户端 SPA）
	// 显式设置则钉死行为，不再探测（ModeSPA 强制 SPA；ModeSSR 强制 SSR/静态）。
	Mode        Mode
	Dev         bool   // true: SSR 错误页显示 goja 栈（esbuild/HMR 由 wf dev 或 dev.DevServer 提供）
	EmbedFS     fs.FS  // 生产环境嵌入的文件系统（//go:embed）
	DistDir     string // 生产产物在 EmbedFS 中的子路径，默认 "dist"
	ServerEntry string // SSR 入口，默认自动探测 src/entry-server.(tsx|ts)
	ModulesDir  string // 依赖目录名，默认 "node_modules"（可改为 "wf_modules" 等）
	// Renderer 动态渲染器。为 nil 时不执行任何 JS：
	// dist 里有 pages/*.html 就按 URL 取预渲染页；只有 index.html 时
	// ServeHTTP 自动走 SPA 外壳。有效形态见 Engine.Mode()。
	Renderer Renderer
	// ClientScript 注入页面的客户端脚本路径，默认 "<BasePath>/assets/client.js"
	// （仅当 EmbedFS 里确实存在该文件时）。设为 "-" 可关闭注入
	// （例如纯静态、无需 hydration 的页面）。
	ClientScript string
}

// Renderer 由 jsruntime 等实现；把"执行 JS"这一层从引擎里解耦出去，
// 这样静态部署的二进制不会链接 JS 引擎。
type Renderer interface {
	RenderHTML(url string, propsJSON []byte) (html, head string, err error)
	Close() error
}

// Page：Go 控制器注入的渲染请求
type Page struct {
	URL       string         // 当前路由
	Props     map[string]any // 强类型数据（序列化进 VM 全局 + __WF_DATA__）
	Head      Head           // SEO 元信息
	Component string         // （预留）组件名
}

type Head struct {
	Title       string
	Description string
}

// Engine 是 webfront 的核心。
type Engine struct {
	cfg      Config
	mode     Mode   // New 解析后的有效模式（Mode() 可见）
	base     string // 归一化后的 BasePath："" 或 "/site"（无尾斜杠）
	renderer Renderer
	clientFS fs.FS    // 生产态嵌入的客户端产物（//go:embed），nil 表示未配置
	assets   sync.Map // 静态资源缓存：name -> *staticAsset（含 gzip 预压缩体）
	dev      bool     // 开发态：渲染失败时把 goja 栈摆到浏览器上（否则只给 500）
}

// New 组装引擎并解析有效运行模式。
// renderer 为空时不跑 JS：有 pages/ 服务预渲染页，只有 index.html 则 ServeHTTP 走 SPA；
// 显式 Mode 可覆盖自动识别。Effective mode 用 Engine.Mode() 读取。
func New(cfg Config) (*Engine, error) {
	// WF_DEV 由 wf dev 注入给子进程：这样不用改代码，wf dev 起来的应用
	// 自动进入"错误可见"模式（与 Vite 的 dev/prod 差异同构）。
	dev := cfg.Dev || os.Getenv("WF_DEV") != ""
	e := &Engine{cfg: cfg, base: NormalizeBase(cfg.BasePath), renderer: cfg.Renderer, dev: dev}
	if cfg.EmbedFS != nil {
		dist := cfg.DistDir
		if dist == "" {
			dist = "dist"
		}
		sub, err := fs.Sub(cfg.EmbedFS, dist)
		if err != nil {
			return nil, fmt.Errorf("embed dist %q: %w", dist, err)
		}
		e.clientFS = sub
	}
	if e.renderer == nil && e.clientFS == nil {
		return nil, fmt.Errorf("webfront: 既没有 Renderer 也没有 EmbedFS，无从取页面")
	}
	e.mode = resolveMode(cfg.Mode, e.renderer, e.clientFS)
	return e, nil
}

// resolveMode 把"零值 = 自动"解析成有效模式。顺序即优先级：
// 显式配置 → 动态 SSR → 预渲染页 → SPA 外壳 → 无产物的静态兜底。
// 探测只读 EmbedFS，不改变 New 的错误契约：产物缺失不会让 New 失败。
func resolveMode(explicit Mode, renderer Renderer, clientFS fs.FS) Mode {
	if explicit != "" {
		return explicit
	}
	if renderer != nil {
		return ModeSSR
	}
	if hasPrerenderedPages(clientFS) {
		return ModeSSR // Renderer == nil + ModeSSR 就是"只服务预渲染页"
	}
	if fileExists(clientFS, "index.html") {
		return ModeSPA
	}
	return ModeSSR // 空 dist / 还没构建：保持旧的请求期 404，不提前报错
}

// hasPrerenderedPages 判断 dist/pages 下是否存在任意 .html。
// 不能只看 pages/index.html：routes.json 未必包含 "/"。
func hasPrerenderedPages(clientFS fs.FS) bool {
	if clientFS == nil {
		return false
	}
	found := false
	_ = fs.WalkDir(clientFS, "pages", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // pages 不存在就是没有预渲染页
		}
		if !d.IsDir() && strings.HasSuffix(path, ".html") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// fileExists 判断嵌入文件系统里的普通文件是否存在；目录不算。
func fileExists(fsys fs.FS, name string) bool {
	if fsys == nil {
		return false
	}
	fi, err := fs.Stat(fsys, name)
	return err == nil && !fi.IsDir()
}

// Mode 返回 New 解析后的有效运行模式（零值 Mode 也会被解析成具体值）。
// ModeSSR + Static()==true 表示预渲染/静态；ModeSSR + Static()==false 表示动态 SSR。
func (e *Engine) Mode() Mode { return e.mode }

// NormalizeBase 把用户写的 BasePath 归一化成 "" 或 "/prefix"（无尾斜杠）。
// 导出是为了让 CLI（wf build -base）与引擎用同一套规则，避免两处不一致。
func NormalizeBase(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

// BasePath 返回归一化后的挂载前缀（挂根时为空串）。
func (e *Engine) BasePath() string { return e.base }

// stripBase 把请求路径还原成"前端视角"的路径：/site/about → /about。
// 不在前缀下的路径原样返回（由调用方决定是 404 还是照常服务）。
func (e *Engine) stripBase(p string) string {
	if e.base == "" {
		return p
	}
	if p == e.base {
		return "/"
	}
	if strings.HasPrefix(p, e.base+"/") {
		return p[len(e.base):]
	}
	return p
}

// Static 表示当前引擎没有 JS 渲染器（预渲染 / 纯静态产物）。
// 自动模式下 Renderer == nil 也可能解析成 ModeSPA：判断有效形态请用 Mode()。
func (e *Engine) Static() bool { return e.renderer == nil }

// Close 释放渲染器（若有）。
func (e *Engine) Close() error {
	if e.renderer != nil {
		return e.renderer.Close()
	}
	return nil
}

// RenderHTML 渲染成完整 HTML 字符串（不写 ResponseWriter，预渲染也用它）。
// url 传"前端视角"的路径（如 "/about"），不要带 BasePath —— 预渲染产物里存的就是这个。
func (e *Engine) RenderHTML(url string, page Page) (string, error) {
	if e.renderer == nil {
		return "", fmt.Errorf("webfront: 静态模式无渲染器（页面应由 wf build -prerender 预先生成）")
	}
	if url == "" {
		url = page.URL
	}
	propsJSON, err := json.Marshal(page.Props)
	if err != nil {
		return "", err
	}
	html, head, err := e.renderer.RenderHTML(url, propsJSON)
	if err != nil {
		return "", err
	}
	return e.assemble(head, html, page), nil
}

// assemble 拼装完整 HTML：head + hydration 数据 + 客户端脚本 + SEO 元信息与样式自动补齐。
func (e *Engine) assemble(head, html string, page Page) string {
	// 1. 若前端 head 未定义 <title>，自动使用 page.Head.Title 补齐
	if page.Head.Title != "" && !strings.Contains(head, "<title") {
		head = "<title>" + htmlpkg.EscapeString(page.Head.Title) + "</title>" + head
	}
	// 2. 若前端 head 未定义 description，自动使用 page.Head.Description 补齐
	if page.Head.Description != "" && !strings.Contains(head, "name=\"description\"") && !strings.Contains(head, "name='description'") {
		head += `<meta name="description" content="` + htmlpkg.EscapeString(page.Head.Description) + `">`
	}
	// 3. 若 clientFS 中存在 assets/client.css 且 head 中未引入，自动注入样式链接
	if e.hasClientCSS() && !strings.Contains(head, "assets/client.css") {
		head += `<link rel="stylesheet" href="` + e.base + `/assets/client.css">`
	}

	// json.Marshal 默认转义 <>&，防 XSS（§6 #5）
	dataJSON, _ := json.Marshal(page.Props)
	doc := "<!doctype html><html><head>" + head +
		`<script id="__WF_DATA__" type="application/json">` + string(dataJSON) + `</script>` +
		"</head><body>" + html + "</body></html>"

	// type="module"：代码分割产物是 ESM；模块脚本天然 defer，不阻塞解析；
	// 未拆分的 IIFE 在模块里同样可以执行，两种产物共用同一个标签。
	if src := e.clientScript(); src != "" {
		doc = strings.Replace(doc, "</body>",
			`<script type="module" src="`+src+`"></script></body>`, 1)
	}
	return doc
}

func (e *Engine) hasClientCSS() bool {
	if e.clientFS == nil {
		return false
	}
	f, err := e.clientFS.Open("assets/client.css")
	if err == nil {
		_ = f.Close()
		return true
	}
	return false
}

func (e *Engine) clientScript() string {
	if e.cfg.ClientScript == "-" {
		return ""
	}
	if e.cfg.ClientScript != "" {
		// 显式配置的路径也补前缀，但已经带了就不重复补。
		if e.base != "" && strings.HasPrefix(e.cfg.ClientScript, "/") &&
			!strings.HasPrefix(e.cfg.ClientScript, e.base+"/") {
			return e.base + e.cfg.ClientScript
		}
		return e.cfg.ClientScript
	}
	// 默认路径只在产物里确实有 client.js 时注入：template 模式 / 只嵌 ssr.js
	// 的工程不该拿到一个 404 的 <script>。
	if e.clientFS != nil && fileExists(e.clientFS, "assets/client.js") {
		return e.base + "/assets/client.js"
	}
	return ""
}

// Render 渲染并写响应：动态模式跑 JS；静态模式直接取预渲染页面。
func (e *Engine) Render(w http.ResponseWriter, r *http.Request, page Page) error {
	url := page.URL
	if url == "" && r != nil {
		url = r.URL.Path
	}
	url = e.stripBase(url) // 前端看到的始终是 /about，不是 /site/about
	if e.renderer != nil {
		doc, err := e.RenderHTML(url, page)
		if err != nil {
			// 关键：这里必须自己写响应。调用方普遍写成 `_ = eng.Render(...)`，
			// 若只 return err，浏览器会收到一个空白的 200 —— 也就是"goja 报错了
			// 但页面上什么都看不到"。开发态直接给一张 Vite 风格的错误页。
			e.writeRenderError(w, url, err)
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 渲染成功就显式声明 200。Gin 的 NoRoute 兜底会在进入处理器之前把状态
		// 预设成 404（gin.serveError），若只 Write 不声明，预设的 404 会随首个
		// Write 落盘 —— 页面内容正确、状态码却错。显式 WriteHeader(200) 在落盘
		// 前把它覆盖回来；对标准库这是无害的显式声明。
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(doc))
		return nil
	}
	return e.servePrerendered(w, r, url)
}

// RenderTemplate 把 name 当作"模板名"（SSR 入口里的组件键），data 可以是任意
// 可 JSON 序列化的值（struct / map / nil），返回完整 HTML；不写响应。
// 这是把 React/Vue 当 Go 模板引擎用的快捷入口：
//
//	html, err := eng.RenderTemplate("Home", HomeData{Title: "首页"})
func (e *Engine) RenderTemplate(name string, data any) (string, error) {
	props, err := templateProps(data)
	if err != nil {
		return "", err
	}
	return e.RenderHTML(name, Page{URL: name, Props: props})
}

// RenderTemplateTo 是 RenderTemplate 的 http.Handler 版本：渲染并写响应。
// 序列化/渲染失败时和 Render 一样自己写 500（开发态是错误页），调用方只记日志。
func (e *Engine) RenderTemplateTo(w http.ResponseWriter, r *http.Request, name string, data any) error {
	props, err := templateProps(data)
	if err != nil {
		e.writeRenderError(w, name, err)
		return err
	}
	return e.Render(w, r, Page{URL: name, Props: props})
}

// templateProps 把任意可 JSON 序列化的 data 变成 Page.Props。
// nil → 空对象（模板里可以直接解构，不会拿到 null）。
func templateProps(data any) (map[string]any, error) {
	if data == nil {
		return map[string]any{}, nil
	}
	if m, ok := data.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("webfront: template data: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("webfront: template data 必须是 JSON 对象: %w", err)
	}
	return m, nil
}

// writeRenderError 把 SSR / goja 的失败变成"看得见"的东西。
//   - 开发态（Config.Dev 或 WF_DEV）：整页换成错误页，带上 goja 的栈；
//   - 生产态：只给 500 和一句人话，绝不把内部栈泄露给终端用户。
func (e *Engine) writeRenderError(w http.ResponseWriter, url string, err error) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Cache-Control", "no-store")
	if !e.dev {
		http.Error(w, "webfront: 页面渲染失败（详情见服务端日志）", http.StatusInternalServerError)
		return
	}
	log.Printf("[webfront] SSR %s 渲染失败:\n%v", url, err)
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, devui.ErrorPage(devui.PageOptions{
		Title:  "SSR 渲染失败",
		File:   url,
		Detail: err.Error(),
		Hint:   "goja 执行 src/entry-server.* 时抛错 · 开发态错误页，生产构建不会出现",
	}))
}

// servePrerendered 静态模式：/ → pages/index.html，/about → pages/about.html。
func (e *Engine) servePrerendered(w http.ResponseWriter, r *http.Request, url string) error {
	p := strings.Trim(url, "/")
	candidates := []string{"pages/index.html"}
	if p != "" {
		candidates = []string{"pages/" + p + ".html", "pages/" + p + "/index.html"}
	}
	for _, name := range candidates {
		a, err := e.asset(name)
		if err != nil {
			continue
		}
		e.serveAsset(w, r, a, "public, max-age=60")
		return nil
	}
	http.Error(w, "404 page not found: "+url+"（预渲染产物里没有这个路由）", http.StatusNotFound)
	return nil
}

// serveSPA 纯客户端渲染：命中嵌入文件就直出，否则回 index.html（History 路由 fallback）。
func (e *Engine) serveSPA(w http.ResponseWriter, r *http.Request) {
	if e.clientFS == nil {
		http.Error(w, "webfront: SPA 模式需要 EmbedFS（wf build 产出的 dist）", http.StatusNotFound)
		return
	}
	name := strings.TrimPrefix(e.stripBase(r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if a, err := e.asset(name); err == nil {
			e.serveAsset(w, r, a, "public, max-age=3600")
			return
		}
	}
	// 未知路径一律回 SPA 外壳，交给浏览器端路由。
	a, err := e.asset("index.html")
	if err != nil {
		http.Error(w, "webfront: dist 里没有 index.html（先跑 wf build）", http.StatusNotFound)
		return
	}
	e.serveAsset(w, r, a, "no-cache")
}

// SPAHandler 返回"文件直出 + index.html History fallback"的 handler。
// 它与 Mode / Renderer 无关，因此同一个 Engine 可以默认走 SSR，
// 再把某个子路径交给它：
//
//	mux.Handle("/assets/", eng.StaticHandler())
//	mux.Handle("/app/", eng.SPAHandler()) // 纯客户端路由
//	mux.Handle("/", eng)                  // SSR
//
// 需要 EmbedFS 的 dist 里有 index.html；路径按 Config.BasePath 剥离，
// 缓存策略与 ModeSPA 完全相同（真实文件 1h，外壳 no-cache）。
func (e *Engine) SPAHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.inBase(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		e.serveSPA(w, r)
	})
}

// StaticHandler 见 static.go（带 gzip 预压缩 + ETag 缓存）。

// inBase 判断请求路径是否落在 Config.BasePath 下。
func (e *Engine) inBase(p string) bool {
	return e.base == "" || p == e.base || strings.HasPrefix(p, e.base+"/")
}

// ServeHTTP 实现标准库 http.Handler，框架无关。
// 有效模式由 New 解析：ModeSPA 走 SPA 分支，其余（SSR / 静态 / Islands）都走 Render。
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 前缀不匹配的请求不该由本引擎接管（例如挂在 /site，却收到 /other）。
	if !e.inBase(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	if e.mode == ModeSPA {
		e.serveSPA(w, r)
		return
	}
	// ModeSSR / 静态 / ModeIslands 占位（退化为 SSR）都走同一条渲染路径。
	_ = e.Render(w, r, Page{
		URL:   r.URL.Path,
		Props: map[string]any{"title": "webfront SSR", "path": e.stripBase(r.URL.Path)},
	})
}
