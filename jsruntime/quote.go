// Package jsruntime 在 Go 里执行 SSR bundle，产出 HTML 字符串。
//
// 单独成包是为了"按需付费"：JS 引擎很贵（goja ≈ +9.5MB），只有需要**运行期动态渲染**
// 的服务才该链接它。纯静态（预渲染）部署只 import webfront，二进制 ≈ 6.9MB。
//
// 引擎：goja（纯 Go，零 cgo）。若遇到 goja 的兼容性硬伤，可另接一个完整 ES2023 的
// 纯 Go 后端（≈ +26MB）——只需实现同一个 Renderer 接口，上层零改动，
// 详见 ARCHITECTURE.md §5.6「备选方案」。
package jsruntime

import "fmt"

// quote 生成 JS 字符串字面量。
func quote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if r < 0x20 {
				b = append(b, []byte(fmt.Sprintf("\\u%04x", r))...)
			} else {
				b = append(b, []byte(string(r))...)
			}
		}
	}
	b = append(b, '"')
	return string(b)
}
