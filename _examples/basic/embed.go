//go:build !dev

package main

import "embed"

// 生产构建把前端产物打进二进制。
//
// dev 构建由 dev.Handler 接管，不需要 dist 存在；对应的空 FS 桩在
// embed_dev.go。两个文件都放在 main 包里，因为 //go:embed 只能嵌入
// 同目录或子目录。
//
//go:embed all:web/dist
var distFS embed.FS
