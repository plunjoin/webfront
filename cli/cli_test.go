package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run 的契约：不 panic、不 os.Exit，用返回值表达结果——这样嵌进别人的
// 进程里时，一个写错的参数不会把宿主带走。
func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, 1},
		{"help", []string{"help"}, 0},
		{"-h", []string{"-h"}, 0},
		{"--help", []string{"--help"}, 0},
		{"unknown command", []string{"nope"}, 1},
		{"bad flag", []string{"build", "-nope"}, 2},
		{"flags after root", []string{"build", "some-dir", "-mode", "spa"}, 2},
		{"subcommand help", []string{"build", "-h"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Run(c.args); got != c.want {
				t.Fatalf("Run(%q) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}

func TestWebDirOf(t *testing.T) {
	// 不存在的目录原样当作前端目录；存在 web/ 子目录时取 web/（由调用点的示例覆盖）。
	if got := webDirOf(t.TempDir()); got == "" {
		t.Fatal("webDirOf returned empty path")
	}
}

func TestResolveModules(t *testing.T) {
	d := t.TempDir()

	if got := resolveModules(d, "custom"); got != "custom" {
		t.Fatalf("显式目录被改写: %q", got)
	}
	if got := resolveModules(d, "auto"); got != "wf_modules" {
		t.Fatalf("空项目应回退到 wf_modules, got %q", got)
	}

	// 只有 node_modules（npm 平铺布局）且里面有内容时，应探测到它。
	if err := os.MkdirAll(filepath.Join(d, "node_modules", "react"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveModules(d, ""); got != "node_modules" {
		t.Fatalf("应探测到 node_modules, got %q", got)
	}

	// 一旦出现版本化 store，优先 wf_modules。
	store := filepath.Join(d, "wf_modules", ".store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "manifest.json"), []byte(`{"root":{},"packages":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveModules(d, ""); got != "wf_modules" {
		t.Fatalf("有 store 时应优先 wf_modules, got %q", got)
	}

	// 空的 wf_modules（中断 install 留下）不应遮住旁边可用的 node_modules。
	d2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d2, "wf_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d2, "node_modules", "react"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveModules(d2, ""); got != "node_modules" {
		t.Fatalf("空 wf_modules 不应遮住 node_modules, got %q", got)
	}

	// 只有 .store（install 中断，manifest 未写）也一样。
	d3 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d3, "wf_modules", ".store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d3, "node_modules", "react"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := resolveModules(d3, ""); got != "node_modules" {
		t.Fatalf("只有 .store 的 wf_modules 不应遮住 node_modules, got %q", got)
	}
}

func TestResolvePrerender(t *testing.T) {
	d := t.TempDir()

	if p, auto := resolvePrerender(d, "auto"); p != "" || auto {
		t.Fatalf("没有 routes.json 时不应预渲染: %q %v", p, auto)
	}
	if err := os.WriteFile(filepath.Join(d, "routes.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, auto := resolvePrerender(d, ""); p == "" || !auto {
		t.Fatalf("存在 routes.json 时应自动预渲染: %q %v", p, auto)
	}
	if p, _ := resolvePrerender(d, "-"); p != "" {
		t.Fatalf("-prerender - 应关闭预渲染, got %q", p)
	}
	if p, auto := resolvePrerender(d, "x.json"); p != "x.json" || auto {
		t.Fatalf("显式路径应原样返回: %q %v", p, auto)
	}
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{"ssr", "spa", "template"} {
		if !validMode(m) {
			t.Fatalf("validMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "dynamic", "SSR"} {
		if validMode(m) {
			t.Fatalf("validMode(%q) = true, want false", m)
		}
	}
}

func TestCanCleanOutput(t *testing.T) {
	web := t.TempDir()
	if !canCleanOutput(web, filepath.Join(web, "dist")) {
		t.Fatal("web/dist 应允许清理")
	}
	if !canCleanOutput(web, filepath.Join(web, "public", "static")) {
		t.Fatal("web 的子目录应允许清理")
	}
	if canCleanOutput(web, web) {
		t.Fatal("前端工程根目录不应被清理")
	}
	if canCleanOutput(web, filepath.Dir(web)) {
		t.Fatal("工程外目录不应被清理")
	}
}

func TestPagesStale(t *testing.T) {
	cases := []struct {
		name        string
		prerender   string
		hasServer   bool
		prerendered int
		want        bool
	}{
		{"未配置预渲染", "", true, 0, true},
		{"没有 SSR 入口", "web/routes.json", false, 0, true},
		{"routes 为空", "web/routes.json", true, 0, true},
		{"正常预渲染", "web/routes.json", true, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pagesStale(c.prerender, c.hasServer, c.prerendered); got != c.want {
				t.Fatalf("pagesStale(%q, %v, %d) = %v, want %v", c.prerender, c.hasServer, c.prerendered, got, c.want)
			}
		})
	}
}

func TestRemoveStale(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "pages", "index.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "ssr.js"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeStale(d, "pages", "ssr.js", "does-not-exist")

	for _, name := range []string{"pages", "ssr.js"} {
		if _, err := os.Stat(filepath.Join(d, name)); !os.IsNotExist(err) {
			t.Fatalf("%s 应被清理", name)
		}
	}
	if _, err := os.Stat(filepath.Join(d, "keep.txt")); err != nil {
		t.Fatalf("不应删除非构建产物: %v", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 端到端验证构建清理：旧 ssr.js / pages/ / client.css 不能留到 SPA 产物里。
func TestRunBuildCleansStaleArtifacts(t *testing.T) {
	web := filepath.Join(t.TempDir(), "web")
	writeTestFile(t, filepath.Join(web, "src", "entry-client.tsx"), "document.getElementById('root')\n")
	writeTestFile(t, filepath.Join(web, "src", "entry-server.tsx"), "export function render(){ return { html: 'x', head: '' } }\n")
	writeTestFile(t, filepath.Join(web, "dist", "ssr.js"), "old")
	writeTestFile(t, filepath.Join(web, "dist", "pages", "old.html"), "old")
	writeTestFile(t, filepath.Join(web, "dist", "assets", "client.css"), "body{}")

	if code := Run([]string{"build", "-mode", "spa", web}); code != 0 {
		t.Fatalf("build exit = %d", code)
	}
	for _, p := range []string{"ssr.js", "pages", "assets/client.css"} {
		if _, err := os.Stat(filepath.Join(web, "dist", filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Fatalf("%s 应被清理（err=%v）", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(web, "dist", "index.html")); err != nil {
		t.Fatalf("index.html 未生成: %v", err)
	}
}

// -out 指到前端源码目录时，宁可留下旧文件，也不能删用户文件。
func TestRunBuildTemplateOutSourceDirKeepsFiles(t *testing.T) {
	web := filepath.Join(t.TempDir(), "web")
	writeTestFile(t, filepath.Join(web, "src", "entry-server.tsx"), "export function render(){ return { html: 'x', head: '' } }\n")
	writeTestFile(t, filepath.Join(web, "index.html"), "source-index")
	writeTestFile(t, filepath.Join(web, "assets", "logo.txt"), "logo")
	writeTestFile(t, filepath.Join(web, "pages", "hand.html"), "hand")

	if code := Run([]string{"build", "-mode", "template", "-out", web, web}); code != 0 {
		t.Fatalf("build exit = %d", code)
	}
	for _, p := range []string{"index.html", "assets/logo.txt", "pages/hand.html", "ssr.js"} {
		if _, err := os.Stat(filepath.Join(web, filepath.FromSlash(p))); err != nil {
			t.Fatalf("%s 不应被删除: %v", p, err)
		}
	}
}

// public/ 是构建期静态资源目录：wf build 结束时原样复制进 dist，
// 与构建产物同名时以 public 为准。
func TestRunBuildCopiesPublic(t *testing.T) {
	web := filepath.Join(t.TempDir(), "web")
	writeTestFile(t, filepath.Join(web, "src", "entry-client.tsx"), "document.getElementById('root')\n")
	writeTestFile(t, filepath.Join(web, "public", "brand", "logo.svg"), "<svg>logo</svg>")
	writeTestFile(t, filepath.Join(web, "public", "players", "p1.json"), `{"id":1}`)
	writeTestFile(t, filepath.Join(web, "public", "index.html"), "public-index")

	if code := Run([]string{"build", "-mode", "spa", web}); code != 0 {
		t.Fatalf("build exit = %d", code)
	}
	for path, want := range map[string]string{
		"brand/logo.svg":  "<svg>logo</svg>",
		"players/p1.json": `{"id":1}`,
		"index.html":      "public-index", // 同名时 public 覆盖 wf build 生成的 SPA 外壳
	} {
		b, err := os.ReadFile(filepath.Join(web, "dist", filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("%s 未复制: %v", path, err)
		}
		if string(b) != want {
			t.Fatalf("%s = %q, want %q", path, b, want)
		}
	}
}

// template 模式也复制 public/：它只产 ssr.js，但静态资源仍要进 embed 的 dist。
func TestRunBuildTemplateCopiesPublic(t *testing.T) {
	web := filepath.Join(t.TempDir(), "web")
	writeTestFile(t, filepath.Join(web, "src", "entry-server.tsx"), "export function render(){ return { html: 'x', head: '' } }\n")
	writeTestFile(t, filepath.Join(web, "public", "robots.txt"), "User-agent: *\n")

	if code := Run([]string{"build", "-mode", "template", web}); code != 0 {
		t.Fatalf("build exit = %d", code)
	}
	b, err := os.ReadFile(filepath.Join(web, "dist", "robots.txt"))
	if err != nil || string(b) != "User-agent: *\n" {
		t.Fatalf("dist/robots.txt = %q, %v", b, err)
	}
}

func TestRunBuildNoPublic(t *testing.T) {
	web := filepath.Join(t.TempDir(), "web")
	writeTestFile(t, filepath.Join(web, "src", "entry-client.tsx"), "document.getElementById('root')\n")
	writeTestFile(t, filepath.Join(web, "public", "logo.svg"), "<svg>logo</svg>")

	if code := Run([]string{"build", "-mode", "spa", "-no-public", web}); code != 0 {
		t.Fatalf("build exit = %d", code)
	}
	if _, err := os.Stat(filepath.Join(web, "dist", "logo.svg")); !os.IsNotExist(err) {
		t.Fatalf("logo.svg 不应被复制（err=%v）", err)
	}
	b, err := os.ReadFile(filepath.Join(web, "dist", "index.html"))
	if err != nil || !strings.Contains(string(b), "/assets/client.js") {
		t.Fatalf("index.html 应是构建生成的 SPA 外壳: %q, %v", b, err)
	}
}

// 产物目录落在 public/ 内时拒绝复制：否则边遍历边写入会无限递归。
func TestCopyPublicRejectsOutputInsidePublic(t *testing.T) {
	web := t.TempDir()
	writeTestFile(t, filepath.Join(web, "public", "a.txt"), "a")
	if _, _, err := copyPublic(filepath.Join(web, "public"), filepath.Join(web, "public", "dist")); err == nil {
		t.Fatal("产物目录位于 public 内时应报错")
	}
}

func TestHasInstalledDeps(t *testing.T) {
	d := t.TempDir()
	if hasInstalledDeps(d, "wf_modules") {
		t.Fatal("空目录不应被当成已安装")
	}
	if err := os.MkdirAll(filepath.Join(d, "wf_modules", "react@18.3.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasInstalledDeps(d, "wf_modules") {
		t.Fatal("有内容的目录应被当成已安装")
	}
}
