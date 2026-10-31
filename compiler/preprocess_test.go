package compiler

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/plunjoin/webfront/jsruntime"
)

// preprocessorModules 返回一个装了指定预处理器的依赖目录。
// 和 vue_ts_test.go 一样依赖 _examples/basic/web/wf_modules 里的真实安装；
// 没装就跳过（不联网、不假造包）。
func preprocessorModules(t *testing.T, pkg string) string {
	t.Helper()
	mods := exampleModules(t)
	if _, err := os.Stat(filepath.Join(mods, pkg)); err == nil {
		return mods
	}
	web, _ := filepath.Abs("../_examples/basic/web")
	if d := StorePackageDir(web, mods, pkg); d != "" {
		return mods
	}
	t.Skipf("%s 未安装，跳过（npm i -D %s 后 wf install）", pkg, pkg)
	return ""
}

func TestPreprocessKind(t *testing.T) {
	cases := []struct {
		lang string
		kind string
		ok   bool
	}{
		{"", "", true},
		{"css", "", true},
		{"CSS", "", true},
		{"scss", "sass", true},
		{"SCSS", "sass", true},
		{"sass", "sass", true},
		{"less", "less", true},
		{"stylus", "", false},
		{"styl", "", false},
		{"pug", "", false},
	}
	for _, c := range cases {
		kind, ok := preprocessKind(c.lang)
		if kind != c.kind || ok != c.ok {
			t.Fatalf("preprocessKind(%q) = (%q,%v)，期望 (%q,%v)", c.lang, kind, ok, c.kind, c.ok)
		}
	}
}

// 没装 sass 时必须在"用到的那一刻"给出可执行的指引，而不是等到构建深处报看不懂的错。
func TestPreprocessMissingPackage(t *testing.T) {
	_, err := preprocessStyle(t.TempDir(), "node_modules", "scss", ".a { color: red; }", "X.vue")
	if err == nil {
		t.Fatal("没装 sass 应报错")
	}
	if !strings.Contains(err.Error(), "sass") {
		t.Fatalf("错误信息应提示需要 sass，实际: %v", err)
	}
}

func TestPreprocessUnsupportedLang(t *testing.T) {
	if _, err := preprocessStyle(t.TempDir(), "node_modules", "stylus", ".a", "X.vue"); err == nil {
		t.Fatal("stylus 应报不支持")
	}
}

func TestCompileSFCWithSCSS(t *testing.T) {
	mods := preprocessorModules(t, "sass")

	web := t.TempDir()
	srcDir := filepath.Join(web, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 同目录的 partial：验证 @use 的相对解析与下划线前缀
	if err := os.WriteFile(filepath.Join(srcDir, "_vars.scss"), []byte("$brand: #42b883;\n@use \"./deep\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "_deep.scss"), []byte(".deep { margin: 2px; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := `<template><div class="app"><slot/></div></template>
<style lang="scss" scoped>
@use "sass:math";
@use "./vars" as v;
.app {
  color: v.$brand;
  padding: math.div(8px, 2);
  .inner { margin: 1px; }
}
</style>`
	res, err := CompileSFCResult(web, mods, src, filepath.Join(srcDir, "App.vue"))
	if err != nil {
		t.Fatalf("scss 编译失败: %v", err)
	}
	for _, want := range []string{"#42b883", "padding: 4px", ".app .inner", "margin: 2px", "[data-v-"} {
		if !strings.Contains(res.Code, want) {
			t.Fatalf("产物缺少 %q:\n%s", want, res.Code)
		}
	}
	if strings.Contains(res.Code, "@use") {
		t.Fatalf("产物里不应残留 scss 语法:\n%s", res.Code)
	}
}

func TestCompileSFCWithSassIndented(t *testing.T) {
	mods := preprocessorModules(t, "sass")

	src := `<template><p>x</p></template>
<style lang="sass">
$c: green
.a
  color: $c
</style>`
	res, err := CompileSFCResult("../_examples/basic/web", mods, src, "Indented.vue")
	if err != nil {
		t.Fatalf("缩进语法编译失败: %v", err)
	}
	if !strings.Contains(res.Code, "green") {
		t.Fatalf("产物缺少编译结果:\n%s", res.Code)
	}
}

func TestCompileSFCWithLess(t *testing.T) {
	mods := preprocessorModules(t, "less")

	web := t.TempDir()
	srcDir := filepath.Join(web, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "mixins.less"), []byte("@pad: 6px;\n.rounded(@r) { border-radius: @r; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := `<template><div class="app"/></template>
<style lang="less" scoped>
@import "./mixins.less";
.app {
  padding: @pad;
  .rounded(3px);
  .inner { color: red; }
}
</style>`
	res, err := CompileSFCResult(web, mods, src, filepath.Join(srcDir, "Card.vue"))
	if err != nil {
		t.Fatalf("less 编译失败: %v", err)
	}
	for _, want := range []string{"padding: 6px", "border-radius: 3px", ".app .inner", "[data-v-"} {
		if !strings.Contains(res.Code, want) {
			t.Fatalf("产物缺少 %q:\n%s", want, res.Code)
		}
	}
}

func TestCompileSFCSCSSError(t *testing.T) {
	mods := preprocessorModules(t, "sass")

	src := `<template><p>x</p></template>
<style lang="scss">.a { color: $nope; }</style>`
	_, err := CompileSFCResult("../_examples/basic/web", mods, src, "Bad.vue")
	if err == nil {
		t.Fatal("scss 变量未定义应报错")
	}
	if !strings.Contains(err.Error(), "Undefined variable") {
		t.Fatalf("错误信息应来自 sass，实际: %v", err)
	}
	// dart-sass 的 message 只有一句话，行列在 span 里；不带位置的话报错基本没用。
	if !regexp.MustCompile(`\d+:\d+`).MatchString(err.Error()) {
		t.Fatalf("错误信息应带上行:列，实际: %v", err)
	}
}

// 端到端：scss 编译出来的 CSS 必须真的进到 SSR 的 <head> 里，而不只是"编译不报错"。
func TestVueSCSSEndToEnd(t *testing.T) {
	mods := preprocessorModules(t, "sass")

	web := t.TempDir()
	srcDir := filepath.Join(web, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := `<template><h1 class="title">{{ msg }}</h1></template>
<script setup lang="ts">
defineProps<{ msg: string }>()
</script>
<style lang="scss" scoped>
$brand: #42b883;
.title { color: $brand; &:hover { color: darken($brand, 10%); } }
</style>`
	if err := os.WriteFile(filepath.Join(srcDir, "App.vue"), []byte(app), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := `import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render(url: string, props: any) {
  return { html: await renderToString(createSSRApp(App, props)), head: '' }
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "entry-server.ts"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	bundle, err := BuildSSRWith(web, "src/entry-server.ts", mods)
	if err != nil {
		t.Fatalf("BuildSSRWith: %v", err)
	}
	pool, err := jsruntime.New(bundle, 1)
	if err != nil {
		t.Fatalf("jsruntime.New: %v", err)
	}
	defer pool.Close()

	html, head, err := pool.RenderHTML("/", []byte(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "data-v-") {
		t.Fatalf("scoped 属性丢失: %s", html)
	}
	if !strings.Contains(head, "#42b883") {
		t.Fatalf("SSR head 里没有编译后的 scss: %q", head)
	}
	if !strings.Contains(head, ":hover") {
		t.Fatalf("scss 嵌套语法没有被编译: %q", head)
	}
}
