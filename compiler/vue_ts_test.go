package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plunjoin/webfront/jsruntime"
)

// vueTSFixture 是一份典型的 <script setup lang="ts"> SFC：
// 含 interface/泛型/类型断言，覆盖"TS 语法必须被二次转译"的所有关键点。
const vueTSFixture = `<template>
  <div class="app">
    <h1>{{ title }}</h1>
    <p v-for="(it, i) in items" :key="i">{{ it.label as string }}</p>
    <button @click="count++">{{ count }}</button>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'

interface Item { label: string; done?: boolean }
type Props = { title?: string }

const props = withDefaults(defineProps<Props>(), { title: 'Hello TS' })
const count = ref<number>(0)
const items = ref<Item[]>([{ label: 'a' }, { label: 'b', done: true }])
const title = computed<string>(() => ` + "`${props.title} (${count.value})`" + `)
</script>

<style scoped>
.app { color: #333; }
</style>
`

func exampleModules(t *testing.T) string {
	t.Helper()
	mods, err := filepath.Abs("../_examples/basic/web/wf_modules")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mods, ".store", "manifest.json")); err != nil {
		t.Skip("wf_modules 未安装，跳过（先跑 wf install）")
	}
	return mods
}

func TestCompileSFCScriptSetupTS(t *testing.T) {
	mods := exampleModules(t)
	res, err := CompileSFCResult("../_examples/basic/web", mods, vueTSFixture, "App.vue")
	if err != nil {
		t.Fatalf("CompileSFCResult: %v", err)
	}
	if res.Lang != "ts" {
		t.Fatalf("Lang = %q, want ts", res.Lang)
	}
	if SFCLoader(res.Lang) == SFCLoader("js") {
		t.Fatal("ts 产物必须选 TS Loader，否则类型语法不会被擦除")
	}
	if !strings.Contains(res.Code, "const __script =") {
		t.Fatalf("应产出 const __script =（genDefaultAs），实际:\n%s", res.Code)
	}
	if !strings.Contains(res.Code, "it.label as string") {
		t.Fatalf("模板里的 TS 表达式应被保留（交给 esbuild 擦除），实际:\n%s", res.Code)
	}
	if strings.Contains(res.Code, "export default /*@__PURE__*/") || strings.Contains(res.Code, "export default {") {
		t.Fatalf("组件选项不应再以 export default 形式出现（应改写为 const __script），实际:\n%s", res.Code)
	}
}

func TestVueSFCSSREndToEndWithTS(t *testing.T) {
	mods := exampleModules(t)

	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "src", "App.vue"), []byte(vueTSFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := `import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render(url: string, props: Record<string, any>) {
  return { html: await renderToString(createSSRApp(App, props)), head: '<title>ts</title>' }
}
`
	if err := os.WriteFile(filepath.Join(web, "src", "entry-server.ts"), []byte(entry), 0o644); err != nil {
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

	html, _, err := pool.RenderHTML("/", []byte(`{"title":"你好"}`))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(html, "你好 (0)") {
		t.Fatalf("SSR 输出不对: %s", html)
	}
	if !strings.Contains(html, "data-v-") {
		t.Fatalf("scoped 样式作用域丢失: %s", html)
	}
}

func TestCompileSFCImportedPropsType(t *testing.T) {
	mods := exampleModules(t)

	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "src", "types.ts"), []byte("export interface P { x: string; y?: number }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `<template><p>{{ p.x }}</p></template>
<script setup lang="ts">
import type { P } from './types'
const p = defineProps<P>()
</script>`
	res, err := CompileSFCResult(web, mods, src, filepath.Join(web, "src", "Imported.vue"))
	if err != nil {
		t.Fatalf("导入类型解析失败: %v", err)
	}
	if !strings.Contains(res.Code, "x: { type: String, required: true }") {
		t.Fatalf("defineProps<ImportedType> 未生成运行时 props:\n%s", res.Code)
	}
}

func TestCompileSFCReportsTemplateError(t *testing.T) {
	mods := exampleModules(t)
	bad := `<template><div>{{ foo( }}</div></template><script setup lang="ts">const foo = (x: number) => x</script>`
	if _, err := CompileSFC("../_examples/basic/web", mods, bad, "Bad.vue"); err == nil {
		t.Fatal("模板语法错误必须返回 error，而不是产出坏模块")
	}
}

func TestCompileSFCRejectsUnsupportedLangs(t *testing.T) {
	mods := exampleModules(t)
	cases := []string{
		`<template><div/></template><style lang="stylus">.a\n  color red</style>`,
		`<template lang="pug">div</template>`,
		`<template><div/></template><script lang="tsx">export default {}</script>`,
	}
	for _, src := range cases {
		if _, err := CompileSFC("../_examples/basic/web", mods, src, "X.vue"); err == nil {
			t.Fatalf("不支持的 lang 应报错: %s", src)
		}
	}
}
