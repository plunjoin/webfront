//go:build !dev

// 生产构建下的 dev 桩：默认二进制不含 esbuild / fsnotify / HMR。
package main

import (
	"fmt"
	"os"
)

func runDev() {
	fmt.Println("这是生产构建（不含开发服务器）。")
	fmt.Println("开发模式请用：wf dev（或 go run -tags dev . dev）")
	os.Exit(1)
}
