//go:build dev

package dev

// css.go：开发态 CSS 的 @import / url() 改写。
//
// 浏览器把 dev CSS 注入 <style> 后，里面的相对 URL 以“文档 URL”为 base，
// 而不是 CSS 文件 URL；同时 @import 不会经过 ES 模块图。这里把顶层
// @import 变成真正的 JS import（每个 CSS 文件仍是自己的 HMR 边界），
// 把 url() 指向 /@wf/asset/ 的原始字节，语义与生产构建保持一致。

import (
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/plunjoin/webfront/module"
)

// cssImportRule 是一条顶层 @import 在输出 CSS 中的位置与参数。
type cssImportRule struct {
	specifier string // 引号/url() 里的原始 URL
	condition string // media / supports / layer 后缀；为空表示无条件
	start     int
	end       int
}

// transformCSS 把 esbuild 输出的 CSS 改成浏览器能正确加载的形态：
//   - 无条件的本地 @import → JS import（依赖 CSS 自己注入并 self-accept）
//   - 有条件/@deps 的 @import → 保留 CSS @import，但 URL 改成绝对地址
//   - 相对/项目内根路径 url() → /@wf/asset/<id> 原始字节
//
// 返回改写后的 CSS、需要放在模块顶部的 JS import 列表，以及过滤后的图边。
// 条件 @import 不走 JS 模块（媒体条件会被丢掉），因此从图里移除以免
// HMR 广播一个浏览器从未以模块方式加载过的 URL。
func (d *DevServer) transformCSS(css, ownerFull string, imports []module.Import) (string, []string, []module.Import) {
	rules := scanCSSImports(css)
	if len(rules) == 0 && !strings.Contains(strings.ToLower(css), "url(") {
		return css, nil, imports
	}

	bySpecifier := make(map[string]module.Import, len(imports))
	for _, imp := range imports {
		if imp.Kind == "import-rule" {
			bySpecifier[imp.Specifier] = imp
		}
	}
	drop := map[string]bool{}
	var jsImports []string
	var replacements []struct {
		start, end int
		text       string
	}

	for _, rule := range rules {
		imp, ok := bySpecifier[rule.specifier]
		if !ok {
			// esbuild 可能把 url("./x.css") 规范化成 "./x.css"；再按
			// 解析结果兜底一次，避免因为写法差异漏掉图边。
			imp = d.resolveImport(filepath.Dir(ownerFull), rule.specifier, api.ResolveCSSImportRule)
			if imp.Resolved != "" {
				bySpecifier[rule.specifier] = imp
				ok = true
			}
		}
		if !ok || imp.External {
			// 裸包 CSS：dev 的 /@deps 能打包样式子路径；没有条件时转成
			// JS import，跟随模块图执行。带条件的保持 @import 语义。
			if rule.condition == "" && isBareCSSSpecifier(rule.specifier) {
				jsImports = append(jsImports, d.url("/@deps/"+rule.specifier))
				replacements = append(replacements, struct {
					start, end int
					text       string
				}{rule.start, rule.end, ""})
			}
			continue
		}
		if rule.condition == "" {
			jsImports = append(jsImports, d.versionedCSSURL(imp.Resolved))
			replacements = append(replacements, struct {
				start, end int
				text       string
			}{rule.start, rule.end, ""})
			continue
		}
		// 条件 @import：保留 CSS 语义（media/supports/layer），URL 换成
		// 原始 CSS 字节；这类依赖不参与模块 HMR（浏览器不是以 JS 模块加载它）。
		if imp.Resolved != "" {
			raw := d.url("/@wf/asset/" + imp.Resolved)
			replacements = append(replacements, struct {
				start, end int
				text       string
			}{rule.start, rule.end, `@import url(` + strconv.Quote(raw) + `) ` + rule.condition + `;`})
			drop[imp.Specifier+"\x00"+imp.Resolved+"\x00"+imp.Kind] = true
		}
	}

	css = applyCSSReplacements(css, replacements)
	css = d.rewriteCSSURLs(css, ownerFull)

	if len(drop) > 0 {
		kept := imports[:0]
		for _, imp := range imports {
			if drop[imp.Specifier+"\x00"+imp.Resolved+"\x00"+imp.Kind] {
				continue
			}
			kept = append(kept, imp)
		}
		imports = kept
	}
	return css, dedupeStrings(jsImports), imports
}

// versionedCSSURL 返回 CSS 依赖的 canonical dev URL；命中脏表时带 ?t。
// 用绝对 URL 而不是相对说明符：owner 可能通过别名/目录 URL 提供，
// 绝对 URL 同时也保证 HMR 更新路径与模块 URL 完全一致。
func (d *DevServer) versionedCSSURL(id string) string {
	full := filepath.Join(d.root, filepath.FromSlash(id))

	u, ok := d.devURL(full)
	if !ok {
		return ""
	}
	d.hmrMu.RLock()
	v := d.dirty[module.Key(id)]
	d.hmrMu.RUnlock()
	if v > 0 {
		u += "?t=" + strconv.FormatInt(v, 10)
	}
	return u
}

func isBareCSSSpecifier(spec string) bool {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") ||
		strings.Contains(spec, "://") || strings.HasPrefix(spec, "data:") {
		return false
	}
	raw := spec
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.HasSuffix(strings.ToLower(raw), ".css")
}

// rewriteCSSURLs 把 url() 里的项目内相对路径改写成 /@wf/asset/<id>。
// data:/http(s):/协议相对/#片段/var() 一律不动；文件不存在时也保持原样，
// 让浏览器自己报 404，而不是悄悄改成一个更难看懂的错误。
func (d *DevServer) rewriteCSSURLs(css, ownerFull string) string {
	if !strings.Contains(strings.ToLower(css), "url(") {
		return css
	}
	var b strings.Builder
	b.Grow(len(css))
	baseDir := filepath.Dir(ownerFull)
	i := 0
	for i < len(css) {
		if css[i] == '/' && i+1 < len(css) && css[i+1] == '*' {
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				b.WriteString(css[i:])
				break
			}
			end += i + 4
			b.WriteString(css[i:end])
			i = end
			continue
		}
		if css[i] == '"' || css[i] == '\'' {
			_, end, ok := parseCSSString(css, i)
			if !ok {
				b.WriteString(css[i:])
				break
			}
			b.WriteString(css[i:end])
			i = end
			continue
		}
		if isCSSURLStart(css, i) {
			innerStart := i + 4
			end, inner, ok := parseCSSURLToken(css, innerStart)
			if !ok {
				b.WriteByte(css[i])
				i++
				continue
			}
			value := unquoteCSSString(inner)
			repl, changed := d.rewriteCSSURLValue(value, baseDir)
			if !changed {
				b.WriteString(css[i:end])
			} else {
				b.WriteString(`url("`)
				b.WriteString(repl)
				b.WriteString(`")`)
			}
			i = end
			continue
		}
		b.WriteByte(css[i])
		i++
	}
	return b.String()
}

func isCSSURLStart(css string, i int) bool {
	if i+4 > len(css) || !strings.EqualFold(css[i:i+4], "url(") {
		return false
	}
	if i == 0 {
		return true
	}
	c := css[i-1]
	return !(c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}

// parseCSSURLToken 从 url( 后开始解析，返回 ')' 之后的位置与括号内原文。
func parseCSSURLToken(css string, start int) (int, string, bool) {
	i := start
	for i < len(css) && (css[i] == ' ' || css[i] == '\t' || css[i] == '\r' || css[i] == '\n') {
		i++
	}
	if i >= len(css) {
		return 0, "", false
	}
	if css[i] == '"' || css[i] == '\'' {
		quote := css[i]
		i++
		begin := i
		for i < len(css) {
			if css[i] == '\\' {
				i += 2
				continue
			}
			if css[i] == quote {
				inner := css[begin:i]
				i++
				for i < len(css) && css[i] != ')' {
					i++
				}
				if i >= len(css) {
					return 0, "", false
				}
				return i + 1, inner, true
			}
			i++
		}
		return 0, "", false
	}
	begin := i
	for i < len(css) && css[i] != ')' {
		i++
	}
	if i >= len(css) {
		return 0, "", false
	}
	return i + 1, strings.TrimSpace(css[begin:i]), true
}

func unquoteCSSString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func (d *DevServer) rewriteCSSURLValue(value, baseDir string) (string, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "//") ||
		strings.HasPrefix(strings.ToLower(raw), "data:") ||
		strings.Contains(raw, "://") || strings.HasPrefix(strings.ToLower(raw), "var(") ||
		strings.HasPrefix(strings.ToLower(raw), "blob:") {
		return value, false
	}
	ref := raw
	suffix := ""
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		suffix = ref[i:]
		ref = ref[:i]
	}
	if ref == "" {
		return value, false
	}
	if decoded, err := url.PathUnescape(ref); err == nil {
		ref = decoded
	}
	var target string
	if strings.HasPrefix(ref, "/") {
		target = filepath.Join(d.root, filepath.FromSlash(strings.TrimPrefix(ref, "/")))
	} else {
		target = filepath.Join(baseDir, filepath.FromSlash(ref))
	}
	target = filepath.Clean(target)
	if _, ok := relWithin(d.root, target); !ok {
		return value, false
	}
	if fi, err := os.Stat(target); err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
		return value, false
	}
	id, ok := module.CanonicalID(d.root, target)
	if !ok {
		return value, false
	}
	return d.url("/@wf/asset/"+id) + suffix, true
}

// scanCSSImports 找出顶层 @import 语句（跳过注释/字符串/嵌套块），
// 支持 @import "x.css" 与 @import url(x.css) 两种写法。
func scanCSSImports(css string) []cssImportRule {
	var rules []cssImportRule
	depth := 0
	i := 0
	for i < len(css) {
		switch {
		case css[i] == '/' && i+1 < len(css) && css[i+1] == '*':
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				return rules
			}
			i += end + 4
			continue
		case css[i] == '"' || css[i] == '\'':
			i = skipCSSString(css, i)
			continue
		case css[i] == '{':
			depth++
		case css[i] == '}':
			if depth > 0 {
				depth--
			}
		case depth == 0 && css[i] == '@' && hasCSSKeyword(css, i+1, "import"):
			start := i
			pos := i + 1 + len("import")
			for pos < len(css) && isCSSSpace(css[pos]) {
				pos++
			}
			var spec string
			var ok bool
			if pos < len(css) && (css[pos] == '"' || css[pos] == '\'') {
				var end int
				spec, end, ok = parseCSSString(css, pos)
				pos = end
			} else if pos+4 <= len(css) && strings.EqualFold(css[pos:pos+4], "url(") {
				var end int
				var inner string
				end, inner, ok = parseCSSURLToken(css, pos+4)
				spec = unquoteCSSString(inner)
				pos = end
			}
			if !ok {
				i++
				continue
			}
			semi, ok := findCSSStatementEnd(css, pos)
			if !ok {
				i++
				continue
			}
			cond := strings.TrimSpace(css[pos:semi])
			rules = append(rules, cssImportRule{
				specifier: spec,
				condition: strings.TrimSpace(cond),
				start:     start,
				end:       semi + 1,
			})
			i = semi + 1
			continue
		}
		i++
	}
	return rules
}

func hasCSSKeyword(css string, i int, keyword string) bool {
	if i+len(keyword) > len(css) || !strings.EqualFold(css[i:i+len(keyword)], keyword) {
		return false
	}
	if i+len(keyword) == len(css) {
		return true
	}
	c := css[i+len(keyword)]
	return isCSSSpace(c) || c == '"' || c == '\'' || c == 'u' || c == 'U'
}

func parseCSSString(css string, start int) (string, int, bool) {
	quote := css[start]
	i := start + 1
	begin := i
	for i < len(css) {
		if css[i] == '\\' {
			i += 2
			continue
		}
		if css[i] == quote {
			return css[begin:i], i + 1, true
		}
		i++
	}
	return "", 0, false
}

func skipCSSString(css string, start int) int {
	_, end, ok := parseCSSString(css, start)
	if !ok {
		return len(css)
	}
	return end
}

func findCSSStatementEnd(css string, start int) (int, bool) {
	depth := 0
	i := start
	for i < len(css) {
		switch css[i] {
		case '\\':
			i += 2
			continue
		case '"', '\'':
			i = skipCSSString(css, i)
			continue
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return 0, false
}

func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f'
}

func applyCSSReplacements(css string, repls []struct {
	start, end int
	text       string
}) string {
	if len(repls) == 0 {
		return css
	}
	sort.Slice(repls, func(i, j int) bool { return repls[i].start < repls[j].start })
	var b strings.Builder
	b.Grow(len(css))
	last := 0
	for _, r := range repls {
		if r.start < last || r.end > len(css) {
			continue
		}
		b.WriteString(css[last:r.start])
		b.WriteString(r.text)
		last = r.end
	}
	b.WriteString(css[last:])
	return b.String()
}

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
