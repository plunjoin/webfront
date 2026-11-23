package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

const vueHMRFixture = `<template><button @click="n++">{{ n }}</button></template>
<script setup>
import { ref } from 'vue'
const n = ref(0)
</script>
<style scoped>button { color: green; }</style>`

// HMR 元数据与 accept 回调只能出现在显式开启的 dev 产物里。
// 生产构建（VuePlugin / BuildSSRWith / BuildClient*）走默认分支，必须干净。
func TestCompileSFCHMROnlyWhenRequested(t *testing.T) {
	mods := exampleModules(t)
	web := t.TempDir()
	filename := filepath.Join(web, "src", "App.vue")

	plain, err := CompileSFCResult(web, mods, vueHMRFixture, filename)
	if err != nil {
		t.Fatalf("CompileSFCResult: %v", err)
	}
	for _, banned := range []string{"__hmrId", "__file", "import.meta.hot", "__VUE_HMR_RUNTIME__"} {
		if strings.Contains(plain.Code, banned) {
			t.Fatalf("默认产物不应含 HMR 符号 %q:\n%s", banned, plain.Code)
		}
	}

	dev, err := CompileSFCResultWithOptions(web, mods, vueHMRFixture, filename, SFCOptions{HMR: true})
	if err != nil {
		t.Fatalf("CompileSFCResultWithOptions: %v", err)
	}
	for _, want := range []string{
		"__script.__hmrId",
		"__script.__file",
		"import.meta.hot.accept",
		"__VUE_HMR_RUNTIME__",
		"runtime.reload",
	} {
		if !strings.Contains(dev.Code, want) {
			t.Fatalf("HMR 产物缺少 %q:\n%s", want, dev.Code)
		}
	}
	if dev.Lang != plain.Lang {
		t.Fatalf("HMR 不应改变脚本语言: plain=%q dev=%q", plain.Lang, dev.Lang)
	}
	// __file 是项目相对路径，不能把构建机目录写进浏览器产物。
	if !strings.Contains(dev.Code, `__script.__file = "src/App.vue"`) {
		t.Fatalf("__file 不是项目相对路径:\n%s", dev.Code)
	}
	if strings.Contains(dev.Code, filepath.ToSlash(web)) {
		t.Fatalf("__file 泄露了绝对路径:\n%s", dev.Code)
	}
}

// HMR 代码会先过 goja 的 SFC 编译器，再过 esbuild 的 TS/JS 转译。
// 这里用与 dev.buildModule 相同的关键选项（全 external + banner）验证它
// 能被 esbuild 正常解析，并且 __hmrId 赋值不会因为导出重命名而丢失。
// 只改模板/样式时走 rerender（保留 setup state）；脚本变化才走 reload。
// ScriptHash 必须只覆盖脚本块，模板改动不能改变它。
func TestSFCRerenderOnlyWhenScriptUnchanged(t *testing.T) {
	mods := exampleModules(t)
	web := t.TempDir()
	filename := filepath.Join(web, "src", "App.vue")

	first, err := CompileSFCResultWithOptions(web, mods, vueHMRFixture, filename, SFCOptions{HMR: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ScriptHash == "" {
		t.Fatal("ScriptHash 为空")
	}
	again, err := CompileSFCResultWithOptions(web, mods, vueHMRFixture, filename, SFCOptions{HMR: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.ScriptHash != first.ScriptHash {
		t.Fatal("同一份源码的 ScriptHash 不稳定")
	}

	templateChanged := strings.Replace(vueHMRFixture, "{{ n }}", "{{ n }}!", 1)
	second, err := CompileSFCResultWithOptions(web, mods, templateChanged, filename, SFCOptions{
		HMR:                true,
		PreviousScriptHash: first.ScriptHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ScriptHash != first.ScriptHash {
		t.Fatal("只改模板不应改变 ScriptHash")
	}
	if !strings.Contains(second.Code, "runtime.rerender(") {
		t.Fatalf("模板改动应生成 rerender:\n%s", second.Code)
	}
	// 依赖冒泡/删除引起的更新由服务端标记 mode=reload：即使脚本 hash
	// 没变，也必须跳过 rerender 让 setup 重新执行。
	for _, want := range []string{"ctx.mode === 'reload'", "typeof next.render === 'function'"} {
		if !strings.Contains(second.Code, want) {
			t.Fatalf("rerender 分支缺少 %q:\n%s", want, second.Code)
		}
	}

	scriptChanged := strings.Replace(vueHMRFixture, "const n = ref(0)", "const n = ref(1)", 1)
	third, err := CompileSFCResultWithOptions(web, mods, scriptChanged, filename, SFCOptions{
		HMR:                true,
		PreviousScriptHash: first.ScriptHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.ScriptHash == first.ScriptHash {
		t.Fatal("脚本改动必须改变 ScriptHash")
	}
	if strings.Contains(third.Code, "runtime.rerender(") || !strings.Contains(third.Code, "runtime.reload(") {
		t.Fatalf("脚本改动应生成 reload:\n%s", third.Code)
	}
}

// 生产 client / SSR 产物必须完全干净：HMR 只属于 -tags dev 的 SFC 编译分支。
func TestProductionVueBuildsHaveNoHMRSymbols(t *testing.T) {
	mods := exampleModules(t)
	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(web, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/App.vue", vueHMRFixture)
	write("src/entry-client.js", `import App from "./App.vue"; import { createApp } from "vue"; createApp(App).mount("#app");`)
	write("src/entry-server.js", `import App from "./App.vue"; import { createSSRApp } from "vue"; export const app = createSSRApp(App);`)

	// __file 在 Vue 生产 runtime 里仍有非 HMR 用途，SFC 专项测试已单独
	// 保证默认编译产物不带它；这里只锁定真正的 HMR 协议符号。
	banned := []string{"__hmrId", "import.meta.hot", "__VUE_HMR_RUNTIME__", "createHotContext", "__wfHotCreate"}
	client, err := BuildClientWith(web, "src/entry-client.js", "dist/client.js", mods)
	if err != nil {
		t.Fatalf("BuildClientWith: %v", err)
	}
	ssr, err := BuildSSRWith(web, "src/entry-server.js", mods)
	if err != nil {
		t.Fatalf("BuildSSRWith: %v", err)
	}
	for name, out := range map[string][]byte{"client": client, "ssr": ssr} {
		for _, bad := range banned {
			if strings.Contains(string(out), bad) {
				t.Fatalf("%s 生产产物不应含 HMR 符号 %q", name, bad)
			}
		}
	}
}

func TestCompileSFCHMRSurvivesEsbuild(t *testing.T) {
	mods := exampleModules(t)
	web := t.TempDir()
	filename := filepath.Join(web, "src", "App.vue")
	dev, err := CompileSFCResultWithOptions(web, mods, vueHMRFixture, filename, SFCOptions{HMR: true})
	if err != nil {
		t.Fatalf("CompileSFCResultWithOptions: %v", err)
	}

	res := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   dev.Code,
			ResolveDir: filepath.Dir(filename),
			Sourcefile: "src/App.vue",
			Loader:     SFCLoader(dev.Lang),
		},
		Bundle:        true,
		Write:         false,
		Format:        api.FormatESModule,
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: web,
		Banner: map[string]string{"js": `import { createHotContext } from "/@wf/client";` +
			"\nimport.meta.hot = createHotContext(\"/src/App.vue\");\n"},
		LogLevel: api.LogLevelSilent,
		Plugins: []api.Plugin{{
			Name: "external",
			Setup: func(b api.PluginBuild) {
				b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
					return api.OnResolveResult{Path: args.Path, External: true}, nil
				})
			},
		}},
	})
	if len(res.Errors) > 0 {
		t.Fatalf("HMR 产物无法通过 esbuild: %s", FormatMessages(res.Errors))
	}
	if len(res.OutputFiles) == 0 {
		t.Fatal("esbuild 没有输出")
	}
	out := string(res.OutputFiles[0].Contents)
	for _, want := range []string{"__hmrId", "import.meta.hot.accept", "__VUE_HMR_RUNTIME__"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转译后丢失 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `createHotContext("/src/App.vue")`) {
		t.Fatalf("banner 没有写进产物:\n%s", out)
	}
}
