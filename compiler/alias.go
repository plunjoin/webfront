package compiler

// alias.go：导入别名（@/、tsconfig paths）的解析。
//
// 两处需要它：
//   - wf build：esbuild 自己会读 tsconfig 的 paths，但不认识约定俗成的 "@/"
//     （没有 tsconfig 时），这里用插件补齐，保证开发/生产一致；
//   - wf dev：逐文件 Transform 不解析模块路径，把别名写进 index.html 的
//     import map，浏览器（以及 HMR 的动态 import）就能按 Vite 的习惯工作。
//
// 规则（与 TypeScript / Vite 的常见约定一致）：
//   - 默认 "@/..." → <root>/src/...（项目里存在 src/ 时）；
//   - tsconfig.json / jsconfig.json 的 compilerOptions.paths 优先于默认值；
//   - paths 里的 "prefix/*": ["./dir/*"] 变成前缀映射，精确键变成精确映射；
//   - 支持 extends 链与 project references（tsconfig.app.json 里的 paths 也生效）；
//   - paths 的值相对 baseUrl（未设置时相对该配置文件所在目录）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Alias 一条别名映射。Prefix 带尾斜杠表示前缀匹配（Target 是目录），
// 否则是精确匹配（Target 是文件或目录）。Target 一律是绝对路径。
type Alias struct {
	Prefix string
	Target string
}

// LoadAliases 读取项目根目录的 tsconfig / jsconfig，返回按最长前缀排序的别名表。
// 没有 tsconfig 时仍会给出默认的 "@/ → src/"（如果 src/ 存在）。
func LoadAliases(rootDir string) []Alias {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		abs = rootDir
	}

	found := map[string]string{}
	// tsconfig 优先；两个都有时以 tsconfig 为准。
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		p := filepath.Join(abs, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			collectConfigAliases(p, map[string]bool{}, found)
			break
		}
	}

	// 约定优于配置：没有显式声明 "@/..." 时，把 @/ 指到 src/。
	// 只在 src/ 真的存在时添加，避免给纯静态项目造出一条必然 404 的映射。
	if _, ok := found["@/"]; !ok {
		src := filepath.Join(abs, "src")
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			found["@/"] = src
		}
	}

	out := make([]Alias, 0, len(found))
	for k, v := range found {
		out = append(out, Alias{Prefix: k, Target: v})
	}
	// 最长前缀优先：@components/ 必须排在 @/ 前面。
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Prefix) != len(out[j].Prefix) {
			return len(out[i].Prefix) > len(out[j].Prefix)
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out
}

// ResolveAlias 按最长前缀解析一个导入 specifier；未命中返回 ok=false。
func ResolveAlias(aliases []Alias, spec string) (string, bool) {
	for _, a := range aliases {
		if strings.HasSuffix(a.Prefix, "/") {
			if strings.HasPrefix(spec, a.Prefix) {
				rest := spec[len(a.Prefix):]
				if rest == "" {
					return a.Target, true
				}
				return filepath.Join(a.Target, filepath.FromSlash(rest)), true
			}
			continue
		}
		if spec == a.Prefix {
			return a.Target, true
		}
	}
	return "", false
}

// AliasPlugin 返回 esbuild 插件：把别名 specifier 解析成绝对路径。
// tsconfig paths 本身 esbuild 也能处理；这里主要补默认的 "@/ → src/"，
// 同时让 build 与 dev 使用完全相同的映射表。解析不到具体文件时返回空结果，
// 交回 esbuild 默认解析（错误信息里仍会保留原始 specifier）。
func AliasPlugin(rootDir string) api.Plugin {
	aliases := LoadAliases(rootDir)
	return api.Plugin{
		Name: "webfront-alias",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				p := args.Path
				// 相对路径 / 绝对路径不归别名管。
				if p == "" || strings.HasPrefix(p, ".") || strings.HasPrefix(p, "/") ||
					strings.HasPrefix(p, `\`) || filepath.IsAbs(p) {
					return api.OnResolveResult{}, nil
				}
				target, ok := ResolveAlias(aliases, p)
				if !ok {
					return api.OnResolveResult{}, nil
				}
				if resolved := ResolveSourceFile(target); resolved != "" {
					return api.OnResolveResult{Path: resolved}, nil
				}
				return api.OnResolveResult{}, nil
			})
		},
	}
}

// buildPlugins 是所有业务构建共用的插件顺序：别名 → store → Vue SFC。
func buildPlugins(rootDir, modulesDir string) []api.Plugin {
	plugins := []api.Plugin{AliasPlugin(rootDir)}
	plugins = append(plugins, storePlugins(rootDir, modulesDir)...)
	plugins = append(plugins, VuePlugin(rootDir, modulesDir))
	return plugins
}

// sourceExtensions 是无扩展名导入的补全顺序（dev / build 共用）。
// .vue 放在 .css 前：同名组件与样式并存时优先组件。
var sourceExtensions = []string{".tsx", ".ts", ".jsx", ".js", ".mjs", ".cjs", ".vue", ".css", ".json"}

// ResolveSourceFile 按 Vite 的规则补全一个文件路径：
// 原路径 → 路径+扩展名 → 路径/index+扩展名。找不到返回 ""。
func ResolveSourceFile(base string) string {
	if fi, err := os.Stat(base); err == nil {
		if !fi.IsDir() {
			return base
		}
		// 目录优先按 index 文件解析；下面统一处理。
	} else if filepath.Ext(base) == "" {
		for _, ext := range sourceExtensions {
			if fi, err := os.Stat(base + ext); err == nil && !fi.IsDir() {
				return base + ext
			}
		}
	}
	for _, ext := range sourceExtensions {
		p := filepath.Join(base, "index"+ext)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// tsconfig 解析（JSONC：允许注释与尾逗号）
// ---------------------------------------------------------------------------

type tsConfig struct {
	Extends         json.RawMessage `json:"extends"` // string 或 string[]（TS 5 支持多个父配置）
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
	References []struct {
		Path string `json:"path"`
	} `json:"references"`
}

// collectConfigAliases 递归展开 extends / references，把当前配置的 paths 写入 out。
// 后写入的覆盖先写入的：父配置先展开，子配置随后覆盖；references 与当前配置同理。
func collectConfigAliases(path string, seen map[string]bool, out map[string]string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if seen[abs] {
		return
	}
	seen[abs] = true

	cfg, ok := readTSConfig(abs)
	if !ok {
		return
	}
	dir := filepath.Dir(abs)

	for _, ext := range parseExtends(cfg.Extends) {
		if p := resolveConfigPath(dir, ext); p != "" {
			collectConfigAliases(p, seen, out)
		}
	}
	for _, ref := range cfg.References {
		if p := resolveConfigPath(dir, ref.Path); p != "" {
			collectConfigAliases(p, seen, out)
		}
	}

	base := dir
	if cfg.CompilerOptions.BaseURL != "" {
		base = joinIfRelative(dir, cfg.CompilerOptions.BaseURL)
	}
	for key, targets := range cfg.CompilerOptions.Paths {
		if len(targets) == 0 || targets[0] == "" {
			continue
		}
		target := targets[0]
		if strings.Contains(target, "*") {
			// import map / 前缀解析只能表达"prefix/*"这种尾部通配。
			if !strings.HasSuffix(key, "/*") || !strings.HasSuffix(target, "*") {
				continue
			}
			prefix := strings.TrimSuffix(key, "*")
			out[prefix] = joinIfRelative(base, strings.TrimSuffix(target, "*"))
			continue
		}
		if strings.Contains(key, "*") {
			continue
		}
		out[key] = joinIfRelative(base, target)
	}
}

// joinIfRelative 把 tsconfig 里的相对路径拼到 base 上；绝对路径保持不变
// （filepath.Join 对第二个绝对参数不会丢弃 base，必须自己判断）。
func joinIfRelative(base, p string) string {
	p = filepath.FromSlash(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(base, p))
}

// parseExtends 兼容 "extends": "..." 与 "extends": ["...", "..."]。
func parseExtends(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// resolveConfigPath 把 extends / references 里的路径变成配置文件路径。
// 只处理相对 / 绝对路径；npm 包形式的 extends 交给 IDE 与 tsc 自己处理。
func resolveConfigPath(dir, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		if !strings.HasPrefix(p, ".") {
			return ""
		}
		p = filepath.Join(dir, filepath.FromSlash(p))
	}
	if fi, err := os.Stat(p); err == nil {
		if fi.IsDir() {
			return filepath.Join(p, "tsconfig.json")
		}
		return p
	}
	// extends: "./tsconfig.base" 这类省略 .json 的写法。
	if fi, err := os.Stat(p + ".json"); err == nil && !fi.IsDir() {
		return p + ".json"
	}
	return p
}

func readTSConfig(p string) (tsConfig, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return tsConfig{}, false
	}
	var cfg tsConfig
	if err := json.Unmarshal(stripJSONC(b), &cfg); err != nil {
		return tsConfig{}, false
	}
	return cfg, true
}

// stripJSONC 去掉 // 与 /* */ 注释以及尾逗号，让 json.Unmarshal 能读 tsconfig。
// 字符串内部的内容一律原样保留。
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(b) {
			switch b[i+1] {
			case '/':
				i += 2
				for i < len(b) && b[i] != '\n' {
					i++
				}
				if i < len(b) {
					out = append(out, '\n')
				}
				continue
			case '*':
				i += 2
				for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
					i++
				}
				i++ // 跳过 '/'
				continue
			}
		}
		out = append(out, c)
	}
	return stripTrailingCommas(out)
}

func stripTrailingCommas(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r' || b[j] == '\n') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // 丢掉尾逗号
			}
		}
		out = append(out, c)
	}
	return out
}
