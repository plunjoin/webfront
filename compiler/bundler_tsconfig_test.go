package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

// writeFakeReact 造一个最小 react 包：只要能让 esbuild 解析 "react/jsx-runtime"。
func writeFakeReact(t *testing.T, root string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "node_modules", "react", "package.json"), `{
  "name": "react",
  "version": "0.0.0",
  "main": "index.js",
  "exports": {
    ".": "./index.js",
    "./jsx-runtime": "./jsx-runtime.js"
  }
}`)
	writeTestFile(t, filepath.Join(root, "node_modules", "react", "index.js"),
		`module.exports = { createElement() {} };`)
	writeTestFile(t, filepath.Join(root, "node_modules", "react", "jsx-runtime.js"),
		`export const Fragment = "wf-fragment";
export const jsx = () => "JSX-RUNTIME-MARKER";
export const jsxs = jsx;`)
}

// 别名插件解析出的 .tsx 也必须拿到 tsconfig 的 jsx 设置。
//
// esbuild 的 tsconfig 是按"解析结果所在目录"逐文件查找的；插件（@/ → src/、
// tsconfig paths）直接返回路径时不会附带 TSConfig，于是 `jsx: react-jsx`
// 丢失、退化成 React.createElement（项目里又没 import React，运行时才炸）。
func TestBuildClientAliasTSXUsesTSConfigJSX(t *testing.T) {
	tmp := t.TempDir()
	writeFakeReact(t, tmp)
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  "compilerOptions": {
    "jsx": "react-jsx",
    "baseUrl": ".",
    "paths": { "@shared/*": ["./shared/*"] }
  }
}`)
	writeTestFile(t, filepath.Join(tmp, "shared", "Widget.tsx"),
		`export default function Widget() { return <span>WIDGET</span> }`)
	writeTestFile(t, filepath.Join(tmp, "src", "entry-client.tsx"),
		`import Widget from "@shared/Widget";
export function App() { return <Widget /> }`)

	out, err := BuildClientWith(tmp, "src/entry-client.tsx", "dist/client.js", "node_modules")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	code := string(out)
	if strings.Contains(code, "React.createElement") {
		t.Fatalf("别名解析出的 .tsx 退化成 createElement（tsconfig 没生效）:\n%s", code)
	}
	if !strings.Contains(code, "JSX-RUNTIME-MARKER") {
		t.Fatalf("产物没有走 tsconfig 指定的 react/jsx-runtime:\n%s", code)
	}
}

// 与上一条对照：不传 TsconfigRaw 时确实会退化。这固化了"必须显式传"的原因，
// 也防止未来有人把 tsconfigRaw() 当成可选项删掉。
func TestBuildClientAliasTSXWithoutRawDegrades(t *testing.T) {
	tmp := t.TempDir()
	writeFakeReact(t, tmp)
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  "compilerOptions": {
    "jsx": "react-jsx",
    "baseUrl": ".",
    "paths": { "@shared/*": ["./shared/*"] }
  }
}`)
	writeTestFile(t, filepath.Join(tmp, "shared", "Widget.tsx"),
		`export default function Widget() { return <span>WIDGET</span> }`)
	writeTestFile(t, filepath.Join(tmp, "src", "entry-client.tsx"),
		`import Widget from "@shared/Widget";
export function App() { return <Widget /> }`)

	abs, _ := filepath.Abs(tmp)
	res := api.Build(api.BuildOptions{
		Bundle:        true,
		Write:         false,
		Format:        api.FormatIIFE,
		Platform:      api.PlatformBrowser,
		EntryPoints:   []string{"src/entry-client.tsx"},
		AbsWorkingDir: abs,
		// 故意不传 TsconfigRaw，复现修复前的行为。
		Plugins:  buildPlugins(abs, "node_modules"),
		LogLevel: api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("build failed: %v", res.Errors)
	}
	if code := string(res.OutputFiles[0].Contents); !strings.Contains(code, "React.createElement") {
		t.Fatalf("对照组应当退化，但产物里没有 React.createElement:\n%s", code)
	}
}

func TestTSConfigRawPrefersTSConfigOverJSConfig(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "jsconfig.json"), `{"compilerOptions":{"jsx":"preserve"}}`)
	if raw := tsconfigRaw(tmp); !strings.Contains(raw, "preserve") {
		t.Fatalf("只有 jsconfig 时应读到它: %q", raw)
	}
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{"compilerOptions":{"jsx":"react-jsx"}}`)
	if raw := tsconfigRaw(tmp); !strings.Contains(raw, "react-jsx") {
		t.Fatalf("tsconfig 应优先于 jsconfig: %q", raw)
	}
	if raw := tsconfigRaw(filepath.Join(tmp, "nope")); raw != "" {
		t.Fatalf("没有配置文件时应返回空串: %q", raw)
	}
	_ = os.Remove(filepath.Join(tmp, "tsconfig.json"))
}
