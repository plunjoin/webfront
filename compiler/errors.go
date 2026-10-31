package compiler

import (
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// FormatMessages 把 esbuild 的 api.Message 渲染成人能读的文本（file:line:col
// + 代码框 + notes）。直接 %+v 会打出 `{ID: PluginName: Text:... Location:0xc000...}`
// 这种带指针的结构体字面量，既没有位置也没有代码片段，终端和浏览器里都没法看。
//
// 这里直接用 esbuild 自己的渲染器（和 CLI 输出同一套），不要手写拼接。
func FormatMessages(msgs []api.Message) string {
	out := api.FormatMessages(msgs, api.FormatMessagesOptions{
		Kind:          api.ErrorMessage,
		Color:         false, // 会被打进 HTTP 响应体 / 浏览器覆盖层，不能带 ANSI
		TerminalWidth: 120,
	})
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}
