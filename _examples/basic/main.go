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
//	go build -ldflags "-s -w" -o app.exe .               → 预渲染静态（无引擎，7.0MB）
//	go build -tags dynamic -ldflags "-s -w" -o app.exe . → 动态 SSR / goja（15.6MB）
//	go build -tags dev -ldflags "-s -w" -o app.exe .     → 开发服务器（esbuild + HMR，11.4MB）
//	go run -tags dev .                                   → 开发模式（带 HMR 热更）
//	wf dev                                               → 同上，且改 .go 自动重启
//
// 接线只有 dev.Handler 一处：普通构建原样返回 fallback，-tags dev 构建由
// DevServer 接管。默认构建不 import jsruntime / compiler，也不会因为 import
// dev 而带上 esbuild / fsnotify / websocket（那些文件都带 //go:build dev）。
package main

import (
	"fmt"
	"net/http"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/dev"
)

func main() {
	mux := http.NewServeMux()
	// dev 构建：./web 的前端由 DevServer 接管；生产构建：fallback 原样返回。
	mux.Handle("/", dev.Handler("./web", prodHandler))
	fmt.Println("✅ webfront demo on http://localhost:8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		panic(err)
	}
}

// prodHandler 延迟到普通构建才执行：dev 构建里不会调用它，所以不要求
// web/dist 已经构建好（embed 声明也已拆到 !dev / dev 两个文件）。
func prodHandler() http.Handler {
	eng, err := newEngine()
	if err != nil {
		panic(err)
	}
	// 自动识别后的形态决定启动检查与页面分派。
	desc := modeDesc
	switch {
	case eng.Mode() == webfront.ModeSPA:
		desc = "纯客户端 SPA · 无 JS 引擎"
		if _, err := distFS.ReadFile("web/dist/index.html"); err != nil {
			fmt.Println("⚠ 二进制里没有 SPA 外壳 web/dist/index.html，先构建前端产物（在仓库根执行）：")
			fmt.Println("   go run ./cmd/wf build -mode spa ./_examples/basic")
			fmt.Println("   然后回到本目录重新 go build。")
		}
	case eng.Static():
		if _, err := distFS.ReadDir("web/dist/pages"); err != nil {
			fmt.Println("⚠ 二进制里没有预渲染页面，先构建前端产物（在仓库根执行）：")
			fmt.Println("   go run ./cmd/wf install -modules wf_modules ./_examples/basic")
			fmt.Println("   go run ./cmd/wf build -modules wf_modules -prerender _examples/basic/web/routes.json ./_examples/basic")
			fmt.Println("   然后回到本目录重新 go build。")
		}
	}
	mux := http.NewServeMux()
	// 客户端产物从二进制内直接服务（gzip + ETag），磁盘上无需 web/dist
	mux.Handle("/assets/", eng.StaticHandler())
	if eng.Mode() == webfront.ModeSPA {
		// SPA：命中文件直出，未命中回 index.html（History fallback）。
		mux.Handle("/", eng)
	} else {
		// SSR / 静态：由业务侧决定每页的 Props。
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
	}
	fmt.Println("   形态：" + desc)
	return mux
}
