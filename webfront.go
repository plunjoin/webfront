// Package webfront: a 100% pure-Go front-end runtime.
// No Node.js, no CGO. Embeds as an http.Handler / Gin middleware.
//
// 两种运行形态，链接成本差一个数量级：
//   - 动态 SSR：Config.Renderer = jsruntime.New(ssrBundle, n) → 带 JS 引擎（≈15.6MB）
//   - 预渲染 ：不设 Renderer，直接服务 wf build -prerender 产出的 HTML（≈6.9MB，无引擎）
package webfront

import (
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

type Mode string

const (
	ModeSPA     Mode = "spa"     // 纯客户端渲染（零 JS 引擎开销）
	ModeSSR     Mode = "ssr"     // 全页面服务端渲染 + 客户端 Hydration
	ModeIslands Mode = "islands" // Astro 孤岛模式（默认零 JS，按需水合）
)

// Config：遵循"零配置可用，按需覆盖"
type Config struct {
	RootDir string // 前端工作目录，默认 "./web"
	// BasePath 路由挂载前缀，默认 "/"（即挂根）。
	// 设为 "/site" 时：注入的客户端脚本路径自动带前缀；预渲染页面查找、
	// StaticHandler 取文件都会先剥掉这个前缀。前端代码里仍按 "/about" 写路由，
	// 不需要知道自己被挂在哪儿。
	BasePath    string
	Mode        Mode   // 运行模式，默认 ModeSPA
	Dev         bool   // true: esbuild 实时转译 + fsnotify + HMR
	EmbedFS     fs.FS  // 生产环境嵌入的文件系统（//go:embed）
	DistDir     string // 生产产物在 EmbedFS 中的子路径，默认 "dist"
	ServerEntry string // SSR 入口，默认自动探测 src/entry-server.(tsx|ts)
	ModulesDir  string // 依赖目录名，默认 "node_modules"（可改为 "wf_modules" 等）
	// Renderer 动态渲染器。为 nil 时引擎走"预渲染静态模式"：
	// 直接按 URL 取 <dist>/pages/<path>.html，不再执行任何 JS。
	Renderer Renderer
	// ClientScript 注入页面的客户端脚本路径，默认 "<BasePath>/assets/client.js"。
	// 设为 "-" 可关闭注入（例如纯静态、无需 hydration 的页面）。
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
	base     string // 归一化后的 BasePath："" 或 "/site"（无尾斜杠）
	renderer Renderer
	clientFS fs.FS    // 生产态嵌入的客户端产物（//go:embed），nil 表示未配置
	assets   sync.Map // 静态资源缓存：name -> *staticAsset（含 gzip 预压缩体）
}

// New 组装引擎。renderer 为空时进入静态模式（只服务预渲染页面）。
func New(cfg Config) (*Engine, error) {
	e := &Engine{cfg: cfg, base: NormalizeBase(cfg.BasePath), renderer: cfg.Renderer}
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
	return e, nil
}

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

// Static 表示当前引擎不做运行期渲染。
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
	if e.clientFS != nil {
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

// StaticHandler 见 static.go（带 gzip 预压缩 + ETag 缓存）。

// ServeHTTP 实现标准库 http.Handler，框架无关。
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 前缀不匹配的请求不该由本引擎接管（例如挂在 /site，却收到 /other）。
	if e.base != "" {
		p := r.URL.Path
		if p != e.base && !strings.HasPrefix(p, e.base+"/") {
			http.NotFound(w, r)
			return
		}
	}

	if e.cfg.Mode == ModeSPA {
		e.serveSPA(w, r)
		return
	}
	if e.cfg.Mode == ModeSSR || e.Static() {
		_ = e.Render(w, r, Page{
			URL:   r.URL.Path,
			Props: map[string]any{"title": "webfront SSR", "path": e.stripBase(r.URL.Path)},
		})
		return
	}
	// ModeIslands 尚未实现，退化为 SSR。
	_ = e.Render(w, r, Page{
		URL:   r.URL.Path,
		Props: map[string]any{"title": "webfront SSR", "path": e.stripBase(r.URL.Path)},
	})
}
