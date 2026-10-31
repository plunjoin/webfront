package compiler

// prerender.go：构建期预渲染（SSG）。
//
// 这是"打包后用 Go 做 SSR"最省的做法：把 HTML 在构建期用 JS 引擎一次性生成好，
// 运行期只做静态文件服务 —— 生产二进制里不需要 JS 引擎，体积从 ~33MB 降到 ~6MB。
// 代价：页面内容固定，动态数据要靠客户端 hydration 补（或走 API）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/jsruntime"
)

// Route 一条预渲染路由。
type Route struct {
	Path  string         `json:"path"`
	Props map[string]any `json:"props"`
}

// ReadRoutes 读 routes.json：[{"path":"/","props":{...}}, ...]
func ReadRoutes(file string) ([]Route, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	var routes []Route
	if err := json.Unmarshal(b, &routes); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return routes, nil
}

// Prerender 在 JS 引擎里渲染每条路由 → 写入 outDir/pages/*.html。
// bundle 是已构建好的 SSR 产物（复用 wf build 的那一份，不重复构建）。
func Prerender(bundle []byte, routes []Route, outDir, clientScript string) ([]string, error) {
	return PrerenderWithBase(bundle, routes, outDir, clientScript, "")
}

// PrerenderWithBase 同 Prerender，但会把部署前缀交给引擎：
// 这样自动注入的 <link rel="stylesheet" href="<base>/assets/client.css"> 才带得上前缀。
func PrerenderWithBase(bundle []byte, routes []Route, outDir, clientScript, basePath string) ([]string, error) {
	pool, err := jsruntime.New(bundle, 2)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	if clientScript == "" {
		clientScript = "/assets/client.js"
	}
	cfg := webfront.Config{
		Mode:         webfront.ModeSSR,
		Renderer:     pool,
		ClientScript: clientScript,
		BasePath:     basePath,
	}
	// 把产物目录当作 clientFS：assemble 只有看到 assets/client.css 才会补 <link>，
	// 否则预渲染页面首屏会缺样式（动态 SSR 有 EmbedFS，预渲染这条路径没有）。
	if _, statErr := os.Stat(filepath.Join(outDir, "assets", "client.css")); statErr == nil {
		cfg.EmbedFS = os.DirFS(outDir)
		cfg.DistDir = "."
	}
	eng, err := webfront.New(cfg)
	if err != nil {
		return nil, err
	}
	defer eng.Close()

	var written []string
	for _, rt := range routes {
		if rt.Path == "" {
			continue
		}
		doc, err := eng.RenderHTML(rt.Path, webfront.Page{URL: rt.Path, Props: rt.Props})
		if err != nil {
			return written, fmt.Errorf("prerender %s: %w", rt.Path, err)
		}
		name := PageFile(rt.Path)
		full := filepath.Join(outDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(full, []byte(doc), 0o644); err != nil {
			return written, err
		}
		written = append(written, full)
	}
	return written, nil
}

// PageFile 把 URL 映射成 dist 内的相对文件：/ → pages/index.html，
// /about → pages/about.html，/a/b → pages/a/b.html。
func PageFile(url string) string {
	p := strings.Trim(url, "/")
	if p == "" {
		return "pages/index.html"
	}
	return "pages/" + p + ".html"
}
