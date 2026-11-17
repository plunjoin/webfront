package dev

// cjs.go：/@deps 的 CommonJS 互操作。
//
// esbuild 把 CJS 入口打成 ESM 时只能生成一句 `export default require_x()`：
// CJS 的导出表在运行时才存在，打包器没法凭空写出具名导出。浏览器里
// `import { useState } from "react"` 于是直接报 "does not provide an export
// named 'useState'"。
//
// Vite 走的是另一条路：在调用方把具名导入改写成"默认导入 + 解构"。我们
// 的开发态是"逐文件转译 + import map"，改调用方要自己解析每个源文件的
// import 语句；改依赖这一侧更简单——扫一遍刚刚打出来的 bundle，把入口模块
// 的 exports 赋值补成真正的具名导出：
//
//	const __wf_cjs_dep = require_react();
//	export default __wf_cjs_dep;
//	export const useState = __wf_cjs_dep.useState;
//
// 扫描只跟随"入口模块的 module.exports = require_x()"这一条再导出链，
// 不碰入口顺带 require 进来的其它依赖（否则 react-dom 会凭空多出 react 的
// 导出名，绑到 undefined 上反而更迷惑）。扫不到名字时保持原样：default
// 导出仍然可用，行为和修复前一致。
//
// 认不出的写法（`module.exports = { ... }` 这类运行时才成形的导出表）
// 不会凭空造名字；这种包在 dev 里仍只能用默认导入 + 解构。

import (
	"regexp"
	"sort"
	"strings"
)

var (
	// exports.foo = / module.exports.foo =
	cjsExportDot = regexp.MustCompile(`(?:^|[^\w$])(?:module\s*\.\s*)?exports\s*\.\s*([A-Za-z_$][\w$]*)\s*=`)
	// exports["foo"] = / exports['foo'] =
	cjsExportIndex = regexp.MustCompile(`(?:^|[^\w$])(?:module\s*\.\s*)?exports\s*\[\s*(?:"([^"]+)"|'([^']+)')\s*\]\s*=`)
	// Object.defineProperty(exports, "foo", ...)
	cjsDefineProp = regexp.MustCompile(`Object\s*\.\s*defineProperty\s*\(\s*(?:module\s*\.\s*)?exports\s*,\s*(?:"([^"]+)"|'([^']+)')`)
	// 入口模块的再导出：module.exports = require_x();
	cjsReexport = regexp.MustCompile(`module\s*\.\s*exports\s*=\s*([A-Za-z_$][\w$]*)\s*\(\s*\)\s*;?`)
	// esbuild 为 CJS 入口生成的唯一一行默认导出。
	cjsDefaultExport = regexp.MustCompile(`(?m)^export default ([A-Za-z_$][\w$]*)\(\);[ \t]*$`)
	// 具名导出必须是合法标识符；带连字符之类的键没法写成 `export const`，
	// 只能留在 default 对象上（与浏览器原生行为一致：报错而不是静默）。
	cjsIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
)

// jsReservedWords 不能出现在 `export const <name>` 里的名字。
var jsReservedWords = map[string]bool{
	// 模块是严格模式：eval / arguments 也不能作为 const 绑定名。
	"eval": true, "arguments": true,
	"await": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "debugger": true, "default": true,
	"delete": true, "do": true, "else": true, "enum": true, "export": true,
	"extends": true, "false": true, "finally": true, "for": true,
	"function": true, "if": true, "implements": true, "import": true,
	"in": true, "instanceof": true, "interface": true, "let": true,
	"new": true, "null": true, "package": true, "private": true,
	"protected": true, "public": true, "return": true, "static": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true,
	"with": true, "yield": true,
}

// addCJSInterop 给 CJS 入口的 ESM 产物补上具名导出；不是 CJS 入口时原样返回。
func addCJSInterop(code []byte) []byte {
	s := string(code)
	matches := cjsDefaultExport.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return code
	}
	loc := matches[len(matches)-1] // 只有一个入口，保持和 esbuild 一致取最后一条
	fn := s[loc[2]:loc[3]]

	names := make([]string, 0, 16)
	seen := map[string]bool{}
	collectCJSExports(s, fn, 0, map[string]bool{}, seen, &names)
	if len(names) == 0 {
		return code
	}
	sort.Strings(names)

	const local = "__wf_cjs_dep"
	var sb strings.Builder
	sb.WriteString("const " + local + " = " + fn + "();\n")
	sb.WriteString("export default " + local + ";\n")
	for _, name := range names {
		sb.WriteString("export const " + name + " = " + local + "." + name + ";\n")
	}
	return []byte(s[:loc[0]] + sb.String() + s[loc[1]:])
}

// collectCJSExports 扫描 __commonJS 包装体里的导出赋值，并沿
// `module.exports = require_x()` 再导出链继续找。
func collectCJSExports(s, fn string, depth int, visited map[string]bool, seen map[string]bool, out *[]string) {
	if depth > 8 || visited[fn] {
		return
	}
	visited[fn] = true

	body, ok := commonJSBody(s, fn)
	if !ok {
		return
	}

	add := func(name string) {
		if !validExportName(name) || seen[name] {
			return
		}
		seen[name] = true
		*out = append(*out, name)
	}
	for _, m := range cjsExportDot.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range cjsExportIndex.FindAllStringSubmatch(body, -1) {
		add(m[1] + m[2]) // 两个分支只有一个非空
	}
	for _, m := range cjsDefineProp.FindAllStringSubmatch(body, -1) {
		add(m[1] + m[2])
	}

	// 再导出：module.exports = require_x();（esbuild 把 require() 改成了
	// bundle 内部的 require_x()，正好可以直接顺着找）。
	for _, m := range cjsReexport.FindAllStringSubmatch(body, -1) {
		collectCJSExports(s, m[1], depth+1, visited, seen, out)
	}
}

// commonJSBody 取出 `var <fn> = __commonJS({ ... })` 里花括号内的源码。
// esbuild 的输出是固定格式：包装体结束的 `});` 顶格，并且不会把包内源码
// 重新缩进，所以找第一个顶格的 `\n});\n` 即可。截早了只会少认几个导出名
// （表现的还是修复前的样子），不会改坏产物。
func commonJSBody(s, fn string) (string, bool) {
	header := "var " + fn + " = __commonJS({"
	i := strings.Index(s, header)
	if i < 0 {
		return "", false
	}
	start := i + len(header)
	rel := strings.Index(s[start:], "\n});\n")
	if rel < 0 {
		return "", false
	}
	return s[start : start+rel], true
}

func validExportName(name string) bool {
	if name == "" || name == "default" || name == "__esModule" {
		return false
	}
	return cjsIdent.MatchString(name) && !jsReservedWords[name]
}
