package devui

import (
	"strings"
	"testing"
)

// 覆盖层脚本必须是"自包含 + 可预期"的：占位符被替换、样式内联、
// 三个公开方法都在（dev 客户端靠它们工作）。
func TestClientScript(t *testing.T) {
	js := ClientScript()
	if strings.Contains(js, "__WF_CSS__") {
		t.Fatal("CSS 占位符没有被替换")
	}
	for _, want := range []string{
		"window.__wfOverlay",
		"attachShadow",
		"unhandledrejection",
		".wf-ov-panel",
		"data-act=\"reload\"",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("覆盖层脚本缺少 %q", want)
		}
	}
	// 内联进 <script> 的字符串里不能出现 </script>，否则 HTML 解析器会提前收尾。
	if strings.Contains(strings.ToLower(js), "</script") {
		t.Fatal("脚本里出现了 </script，会截断内联标签")
	}
}

// 错误页要把内容原样呈现，但绝不能把错误文本当 HTML 执行。
func TestErrorPageEscapes(t *testing.T) {
	page := ErrorPage(PageOptions{
		Title:  "SSR 渲染失败",
		File:   "/about?x=<b>",
		Detail: "render: Error: <script>alert(1)</script>\n    at App (ssr.js:1:2)",
		Hint:   "hint",
		Time:   "12:00:00",
	})
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("错误文本被当成 HTML 输出了（XSS）")
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("错误文本应被转义后展示:\n%s", page)
	}
	if !strings.Contains(page, "/about?x=&lt;b&gt;") {
		t.Error("File 字段没有被转义")
	}
	for _, want := range []string{"SSR 渲染失败", "12:00:00", "wf-detail"} {
		if !strings.Contains(page, want) {
			t.Fatalf("错误页缺少 %q", want)
		}
	}
	// 栈帧压暗，首行错误消息保持高亮 —— 和终端里看到的一致。
	if !strings.Contains(page, `<span class="wf-ov-gutter">    at App (ssr.js:1:2)</span>`) {
		t.Fatal("栈帧没有被单独标记")
	}
}

// 没有 Hint / Time 时要有兜底，别渲染出空白的页脚。
func TestErrorPageDefaults(t *testing.T) {
	page := ErrorPage(PageOptions{Detail: "boom"})
	if !strings.Contains(page, "开发态错误页") {
		t.Error("缺省 hint 没生效")
	}
	if !strings.Contains(page, "渲染失败") {
		t.Error("缺省标题没生效")
	}
}
