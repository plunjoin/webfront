package compiler

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

func TestBuildClientFilesIncludesCSSAndAsset(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "src", "entry-client.js"),
		`import "./style.css"; import logo from "./logo.svg"; console.log(logo);`)
	writeTestFile(t, filepath.Join(tmp, "src", "style.css"), "body{color:red}")
	writeTestFile(t, filepath.Join(tmp, "src", "logo.svg"), `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)

	files, err := BuildClientFiles(tmp, "src/entry-client.js", "dist/assets/client.js", "node_modules")
	if err != nil {
		t.Fatalf("BuildClientFiles: %v", err)
	}
	var js, css, asset bool
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.Path, "client.js"):
			js = true
			if strings.Contains(string(f.Contents), "logo.svg") && !strings.Contains(string(f.Contents), "logo-") {
				t.Errorf("JS 里的静态资源没有被重命名为 hash 文件: %s", f.Path)
			}
		case strings.HasSuffix(f.Path, "client.css"):
			css = true
			if !strings.Contains(string(f.Contents), "color:red") {
				t.Errorf("client.css 内容不对: %s", f.Contents)
			}
		case strings.HasSuffix(f.Path, ".svg"):
			asset = true
		}
	}
	if !js || !css || !asset {
		t.Fatalf("产物不完整 js=%v css=%v asset=%v: %+v", js, css, asset, files)
	}
}

func TestBuildClientWithOptionsDevelopmentSourceMap(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "src", "entry.js"),
		`export const mode = process.env.NODE_ENV;`)

	p := DevelopmentProfile()
	p.Minify = false
	out, err := BuildClientWithOptions(tmp, "src/entry.js", "dist/client.js", "node_modules", p)
	if err != nil {
		t.Fatalf("BuildClientWithOptions: %v", err)
	}
	code := string(out)
	if !strings.Contains(code, `"development"`) {
		t.Fatalf("development define 没有生效:\n%s", code)
	}
	if !strings.Contains(code, "sourceMappingURL=data:application/json") {
		t.Fatalf("development source map 没有生效:\n%s", code)
	}
}

func TestBuildSSRWithCSIImport(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "src", "entry-server.js"),
		`import "./style.css"; export function render(){ return { html: "ok", head: "" } }`)
	writeTestFile(t, filepath.Join(tmp, "src", "style.css"), "body{color:red}")

	out, err := BuildSSRWith(tmp, "src/entry-server.js", "node_modules")
	if err != nil {
		t.Fatalf("SSR 入口 import CSS 不应失败: %v", err)
	}
	if !strings.Contains(string(out), "__WF_APP__") {
		t.Fatalf("SSR 产物缺少 __WF_APP__:\n%s", out)
	}
}

func TestBuildSSRWithAssetImportUsesDataURL(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "src", "entry-server.js"),
		`import logo from "./logo.svg"; export function render(){ return { html: logo, head: "" } }`)
	writeTestFile(t, filepath.Join(tmp, "src", "logo.svg"), `<svg xmlns="http://www.w3.org/2000/svg"></svg>`)

	out, err := BuildSSRWith(tmp, "src/entry-server.js", "node_modules")
	if err != nil {
		t.Fatalf("SSR 入口 import 资源不应失败: %v", err)
	}
	if !strings.Contains(string(out), "data:image/svg+xml") {
		t.Fatalf("SSR 资源没有内联成 data URL:\n%s", out)
	}
}

func TestBuildProfileLoadersOverride(t *testing.T) {
	p := DevelopmentProfile()
	p.Loaders = map[string]api.Loader{".svg": api.LoaderText, "txt": api.LoaderText}
	m := p.loaderMap(false)
	if got := m[".svg"]; got != api.LoaderText {
		t.Fatalf(".svg loader = %v, want text override", got)
	}
	if got := m[".txt"]; got != api.LoaderText {
		t.Fatalf(".txt loader 没有按无点形式补全: %v", got)
	}
	if got := m[".png"]; got != api.LoaderFile {
		t.Fatalf(".png loader = %v, want file", got)
	}
	if got := p.loaderMap(true)[".png"]; got != api.LoaderDataURL {
		t.Fatalf("SSR .png loader = %v, want dataurl", got)
	}
}
