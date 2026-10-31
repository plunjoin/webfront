package compiler

import (
	"strings"
	"testing"
)

// TestCompileSFCErrorIsReturnedNotPanicked 固化一条契约：**任何 SFC 输入都只
// 返回 error，不 panic**。dev server 与构建都在同一进程里跑，一次 panic 就会
// 把整个进程带走；goja 的 JS 异常本来就是以 error 返回的，这里连"宿主回调
// 意外 panic"也一并兜住。
func TestCompileSFCErrorIsReturnedNotPanicked(t *testing.T) {
	web := "../_examples/basic/web"
	mods := exampleModules(t)

	cases := []struct {
		name     string
		src      string
		wantErr  bool
		contains string
	}{
		// 空 SFC 由 @vue/compiler-sfc 自己拒绝，同样走 error 返回
		{name: "空文件", src: "", wantErr: true, contains: "At least one"},
		{name: "纯文本", src: "hello world", wantErr: true, contains: "At least one"},
		{name: "只有模板", src: `<template><div>hi</div></template>`},
		{
			name:     "脚本语法错误",
			src:      `<template><div/></template><script setup>const a = ;</script>`,
			wantErr:  true,
			contains: "compileScript",
		},
		{
			name:     "模板表达式错误",
			src:      `<template><div>{{ 1 + }}</div></template>`,
			wantErr:  true,
			contains: "Bad.vue",
		},
		{
			name:     "模板未闭合",
			src:      `<template><div><span></template>`,
			wantErr:  true,
			contains: "Bad.vue",
		},
		{
			// 纯 CSS 的非法声明不报错：compileStyle 只做 SFC 语义变换，不做 CSS 解析，
			// 和 Vite 一致——非法声明由浏览器忽略。scss / less 则会被预处理器抓住。
			name: "纯 CSS 非法声明透传",
			src:  `<template><div/></template><style>.a { color: ; }</style>`,
		},
		{
			name:     "脚本语言不支持",
			src:      `<template><div/></template><script lang="coffee">x</script>`,
			wantErr:  true,
			contains: "lang=",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 即便 CompileSFCResult 内部漏了 recover，这里也能给出可读的失败信息
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("CompileSFCResult 不该 panic，实际: %v", r)
				}
			}()

			res, err := CompileSFCResult(web, mods, c.src, "Bad.vue")
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际成功: %q", res.Code)
				}
				if !strings.Contains(err.Error(), c.contains) {
					t.Fatalf("错误信息应包含 %q，实际: %v", c.contains, err)
				}
				if !strings.HasPrefix(err.Error(), "Bad.vue") {
					t.Fatalf("错误信息应以文件名开头，实际: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if !strings.Contains(res.Code, "export default") {
				t.Fatalf("产物不像一个模块: %q", res.Code)
			}
		})
	}
}
