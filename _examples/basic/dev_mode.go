//go:build dev

// 开发态入口：go run -tags dev . dev
// 单独成文件并加 build tag，保证默认构建（生产二进制）里没有 esbuild/HMR 相关代码。
package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/plunjoin/webfront/dev"
)

func runDev() {
	ds, err := dev.NewDevServer("./web")
	if err != nil {
		panic(err)
	}
	fmt.Println("✅ webfront DEV server (HMR) on http://localhost:8080")
	fmt.Println("   edit web/src/main.tsx and watch hot reload (no refresh)")
	if os.Getenv("WF_DEV") == "" {
		fmt.Println("   提示：用 wf dev 启动还能热更 Go 代码（改 .go 自动重新编译并重启）")
	}
	if err := http.ListenAndServe(":8080", ds.Handler()); err != nil {
		panic(err)
	}
}
