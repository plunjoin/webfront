//go:build dev

// shell.go：开发态 HTML 外壳与页面渲染。
//
// serveIndex 与项目侧 DevServer.RenderPage 共用这里：import map、数据脚本、
// HMR overlay/boot 由服务端注入；调用方只决定文档骨架（ShellFunc / shell
// 文件）或直接使用默认外壳。
package dev

import (
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"net/http"
	"regexp"
	"strings"
)

const (
	defaultShellTitle = "webfront dev (HMR)"

	// 自定义模板可用的原位注入点。没有占位符时，服务端把 head 注入
	// </head> 前、脚本注入 </body> 前，并用 SSR 结果替换空的 #root。
	wfHeadMarker    = "<!--wf-head-->"
	wfBodyMarker    = "<!--wf-body-->"
	wfScriptsMarker = "<!--wf-scripts-->"
)

var (
	htmlTitleRE       = regexp.MustCompile(`(?is)<title[^>]*>.*?</title>`)
	htmlDescriptionRE = regexp.MustCompile(`(?is)<meta[^>]+name=["']description["'][^>]*>`)
	rootEmptyRE       = regexp.MustCompile(`(?is)<div([^>]*\bid=["']root["'][^>]*)>\s*</div>`)
	bodyOpenRE        = regexp.MustCompile(`(?is)<body[^>]*>`)
	headCloseRE       = regexp.MustCompile(`(?is)</head\s*>`)
	bodyCloseRE       = regexp.MustCompile(`(?is)</body\s*>`)

	// 复用 dist/index.html 时剥掉生产客户端标签：它们指向带 hash 的固定
	// 文件，在 dev 下要么 404，要么加载旧 bundle 造成双份应用。
	prodClientScriptRE = regexp.MustCompile(`(?is)<script\b[^>]*\bsrc\s*=\s*["'][^"']*client\.js[^"']*["'][^>]*>\s*</script\s*>`)
	prodClientLinkRE   = regexp.MustCompile(`(?is)<link\b[^>]*\bhref\s*=\s*["'][^"']*client\.(?:js|css)[^"']*["'][^>]*>`)
)

func stripProdClientAssets(in []byte) []byte {
	out := prodClientScriptRE.ReplaceAll(in, nil)
	out = prodClientLinkRE.ReplaceAll(out, nil)
	return out
}

// RenderPage 渲染并写出一张开发页面。
//
// Page.SSR=true 时先走当前 SSR bundle（WithSSR 配置），把返回的 body/head
// 注入外壳；SSR 编译失败时退回 SPA 外壳（覆盖层已收到 error 更新）。若页面
// 标记了 SSR 但根本没配置 WithSSR，会返回 500：静默退化会让配置错误很难发现。
//
// 数据约定与生产引擎一致：
//   - SSR 页面：Page.Props 进入 <script id="__WF_DATA__">，客户端用它 hydration；
//   - SPA 页面：Page.Bootstrap 进入 <script id="__WF_BOOTSTRAP__">，不写
//     __WF_DATA__（否则 hydrate 一个空 root）。
func (d *DevServer) RenderPage(w http.ResponseWriter, r *http.Request, page Page) {
	doc, err := d.renderDocument(r, page)
	if err != nil {
		d.recordError("dev page "+r.URL.Path, err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(doc)
}

func (d *DevServer) renderDocument(r *http.Request, page Page) ([]byte, error) {
	url := page.URL
	if url == "" {
		url = d.canonicalPath(r.URL.Path)
		if url == "" {
			url = "/"
		}
	}
	propsJSON, err := json.Marshal(page.Props)
	if err != nil {
		return nil, fmt.Errorf("dev: 序列化 Page.Props: %w", err)
	}

	ssrHTML, ssrHead := "", ""
	ssr := false
	if page.SSR {
		if d.ssrEntry == "" {
			return nil, fmt.Errorf("dev: Page.SSR=true 需要 WithSSR 配置（entry: %s）", url)
		}
		d.ssrMu.RLock()
		renderer := d.ssrRenderer
		if renderer != nil {
			ssrHTML, ssrHead, err = renderer.RenderHTML(url, propsJSON)
			if err == nil {
				ssr = true
			}
		}
		d.ssrMu.RUnlock()
		if err != nil {
			return nil, fmt.Errorf("dev: SSR 渲染 %s 失败: %w", url, err)
		}
		// renderer 为 nil：初始/最近一次 bundle 构建失败。保留 SPA 外壳，
		// 错误已经通过 HMR error 更新与 /__wf_error 暴露。
	}

	customShell := d.shell != nil
	var shell []byte
	switch {
	case customShell:
		data := ShellData{
			URL:         url,
			SSR:         ssr,
			HTML:        ssrHTML,
			SSRHead:     ssrHead,
			Props:       page.Props,
			Bootstrap:   page.Bootstrap,
			Title:       page.Title,
			Description: page.Description,
			HTMLAttrs:   page.HTMLAttrs,
			BodyAttrs:   page.BodyAttrs,
			HeadHTML:    page.HeadHTML,
			Request:     r,
		}
		shell, err = d.shell(data)
		if err != nil {
			return nil, fmt.Errorf("dev: HTML shell 失败: %w", err)
		}
	case len(d.shellTemplate) > 0:
		shell = append([]byte(nil), d.shellTemplate...)
	default:
		shell = []byte(`<!doctype html><html><head><meta charset="utf-8"><title>` + defaultShellTitle + `</title>` + wfHeadMarker +
			`</head>` + "\n" + `<body>` + "\n" + `<div id="root">` + wfBodyMarker + `</div>` + "\n" + wfScriptsMarker + `</body></html>`)
	}

	doc := string(shell)
	if !customShell {
		// 模板/默认外壳的骨架由服务端补齐；ShellFunc 自己负责这些字段。
		doc = applyPageMeta(doc, page)
	}

	head := ""
	if !customShell {
		head += page.HeadHTML
	}
	head += ssrHead
	head += d.importMapHTML()
	if ssr {
		head += dataScript("__WF_DATA__", page.Props)
	}
	if page.Bootstrap != nil {
		head += dataScript("__WF_BOOTSTRAP__", page.Bootstrap)
	}
	scripts := `<script src="` + d.url("/@wf/overlay.js") + `"></script>` + "\n" +
		`<script type="module" src="` + d.url("/@wf/boot") + `"></script>`

	doc = injectHead(doc, head)
	doc = injectBody(doc, ssrHTML)
	doc = injectScripts(doc, scripts)
	return []byte(doc), nil
}

// importMapHTML 生成 npm 包与别名的 import map。别名放在依赖之后：同名时
// 用户显式配置的别名优先于 npm 包。
func (d *DevServer) importMapHTML() string {
	importMap := map[string]any{"imports": map[string]string{}}
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
	for _, a := range d.aliases {
		if key, val, ok := d.aliasImport(a); ok {
			imports[key] = val
		}
	}
	if len(imports) == 0 {
		return ""
	}
	importMapJSON, _ := json.MarshalIndent(importMap, "  ", "  ")
	return "\n  <script type=\"importmap\">\n  " + string(importMapJSON) + "\n  </script>\n"
}

// dataScript 输出惰性 JSON 数据块。json.Marshal 默认把 <>& 转义成
// </>/&，页面里出现 </script> 也无法提前闭合标签。
func dataScript(id string, value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return `<script id="` + id + `" type="application/json">` + string(b) + `</script>`
}

// applyPageMeta 只作用于模板/默认外壳：替换 title/description、追加
// <html>/<body> 属性。ShellFunc 已拿到同样的字段，由它自行决定位置。
func applyPageMeta(doc string, page Page) string {
	if page.Title != "" {
		titleTag := "<title>" + htmlpkg.EscapeString(page.Title) + "</title>"
		if htmlTitleRE.MatchString(doc) {
			doc = htmlTitleRE.ReplaceAllStringFunc(doc, func(string) string { return titleTag })
		} else {
			doc = injectBeforeClose(doc, headCloseRE, titleTag)
		}
	}
	if page.Description != "" {
		meta := `<meta name="description" content="` + htmlpkg.EscapeString(page.Description) + `">`
		if htmlDescriptionRE.MatchString(doc) {
			doc = htmlDescriptionRE.ReplaceAllStringFunc(doc, func(string) string { return meta })
		} else {
			doc = injectBeforeClose(doc, headCloseRE, meta)
		}
	}
	if attrs := strings.TrimSpace(page.HTMLAttrs); attrs != "" {
		doc = attachTagAttrs(doc, "html", attrs)
	}
	if attrs := strings.TrimSpace(page.BodyAttrs); attrs != "" {
		doc = attachTagAttrs(doc, "body", attrs)
	}
	return doc
}

// injectHead 优先使用 <!--wf-head-->；否则插到 </head> 前。
func injectHead(doc, head string) string {
	if strings.Contains(doc, wfHeadMarker) {
		return strings.Replace(doc, wfHeadMarker, head, 1)
	}
	return injectBeforeClose(doc, headCloseRE, head)
}

// injectBody 优先使用 <!--wf-body-->；否则替换空的 <div id="root"></div>。
// 两者都没有时（自定义骨架没有挂载点）把 SSR 片段放到 <body> 开头，
// 至少不会静默丢内容。
func injectBody(doc, body string) string {
	if strings.Contains(doc, wfBodyMarker) {
		return strings.Replace(doc, wfBodyMarker, body, 1)
	}
	if body == "" {
		return doc
	}
	if loc := rootEmptyRE.FindStringSubmatchIndex(doc); loc != nil {
		attrs := doc[loc[2]:loc[3]]
		return doc[:loc[0]] + "<div" + attrs + ">" + body + "</div>" + doc[loc[1]:]
	}
	if loc := bodyOpenRE.FindStringIndex(doc); loc != nil {
		return doc[:loc[1]] + body + doc[loc[1]:]
	}
	return doc + body
}

// injectScripts 优先使用 <!--wf-scripts-->；否则插到 </body> 前。
func injectScripts(doc, scripts string) string {
	if strings.Contains(doc, wfScriptsMarker) {
		return strings.Replace(doc, wfScriptsMarker, scripts, 1)
	}
	return injectBeforeClose(doc, bodyCloseRE, scripts)
}

func injectBeforeClose(doc string, closeRE *regexp.Regexp, content string) string {
	if content == "" {
		return doc
	}
	if loc := closeRE.FindStringIndex(doc); loc != nil {
		return doc[:loc[0]] + content + doc[loc[0]:]
	}
	return doc + content
}

// attachTagAttrs 把属性片段追加到第一个 <html>/<body> 标签里。片段由调用方
// 原样提供、自行转义（与生产 webfront.Page 的约定一致）。
func attachTagAttrs(doc, tag, attrs string) string {
	lower := strings.ToLower(doc)
	idx := strings.Index(lower, "<"+tag)
	if idx < 0 {
		return doc
	}
	pos := idx + 1 + len(tag)
	if pos >= len(doc) || (doc[pos] != '>' && doc[pos] != ' ' && doc[pos] != '\t' && doc[pos] != '\n' && doc[pos] != '\r') {
		return doc
	}
	end := strings.IndexByte(doc[idx:], '>')
	if end < 0 {
		return doc
	}
	at := idx + end
	return doc[:at] + " " + attrs + doc[at:]
}
