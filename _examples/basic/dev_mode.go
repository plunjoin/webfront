//go:build dev

// 开发态入口：go run -tags dev . dev
// 单独成文件并加 build tag，保证默认构建（生产二进制）里没有 esbuild/HMR 相关代码。
package main

import (
	"fmt"
	"net/http"

	"github.com/plunjoin/webfront/dev"
)

func runDev() {
	ds, err := dev.NewDevServer("./web")
	if err != nil {
		panic(err)
	}
	fmt.Println("✅ webfront DEV server (HMR) on http://localhost:8080")
	fmt.Println("   edit web/src/main.tsx and watch hot reload (no refresh)")
	if err := http.ListenAndServe(":8080", ds.Handler()); err != nil {
		panic(err)
	}
}
