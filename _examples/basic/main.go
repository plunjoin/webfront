// 示例：单文件生产二进制（内嵌产物，零 Node、零 CGO）。
//
// 第 1 步，在仓库根目录构建前端产物（纯 Go，零 Node）：
//
//	go run ./cmd/wf install -modules wf_modules ./_examples/basic
//	go run ./cmd/wf build -modules wf_modules \
//	    -prerender _examples/basic/web/routes.json ./_examples/basic
//
// 第 2 步，在本目录按需选形态：
//
//	go build -ldflags "-s -w" -o app.exe .               → 预渲染静态（无引擎，6.9MB）
//	go build -tags dynamic -ldflags "-s -w" -o app.exe . → 动态 SSR / goja（15.6MB）
//	go build -tags dev -ldflags "-s -w" -o app.exe .     → 开发服务器（esbuild + HMR，11.4MB）
//	go run -tags dev . dev                               → 开发模式（带 HMR 热更）
//
// 默认构建 **不 import** jsruntime / compiler / dev：那会把 JS 引擎（9.5MB）和
// esbuild 一起链进二进制。构建统一走 wf CLI。
package main

import (
	"embed"
	"fmt"
	"net/http"
	"os"

	"github.com/plunjoin/webfront"
)

//go:embed all:web/dist
var distFS embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dev" {
		runDev()
		return
	}
	runSSR()
}

func runSSR() {
	eng, err := newEngine()
	if err != nil {
		panic(err)
	}
	if eng.Static() {
		if _, err := distFS.ReadFile("web/dist/pages/index.html"); err != nil {
			fmt.Println("⚠ 二进制里没有预渲染页面，先构建前端产物（在仓库根执行）：")
			fmt.Println("   go run ./cmd/wf install -modules wf_modules ./_examples/basic")
			fmt.Println("   go run ./cmd/wf build -modules wf_modules -prerender _examples/basic/web/routes.json ./_examples/basic")
			fmt.Println("   然后回到本目录重新 go build。")
		}
	}
	mux := http.NewServeMux()
	// 客户端产物从二进制内直接服务（gzip + ETag），磁盘上无需 web/dist
	mux.Handle("/assets/", eng.StaticHandler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = eng.Render(w, r, webfront.Page{
			URL: r.URL.Path,
			Props: map[string]any{
				"title": "webfront SSR Demo",
				"name":  "零 Node 世界",
				"path":  r.URL.Path,
			},
		})
	})
	fmt.Println("✅ webfront demo on http://localhost:8080  （" + modeDesc + "）")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}
