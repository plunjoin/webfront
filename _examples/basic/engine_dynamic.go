//go:build dynamic

// 动态 SSR 构建：go build -tags dynamic .
// 带上 goja 引擎，每个请求实时渲染（≈15.6MB）。
package main

import (
	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/jsruntime"
)

const modeDesc = "动态 SSR · 带 JS 引擎"

func newEngine() (*webfront.Engine, error) {
	ssr, err := distFS.ReadFile("web/dist/ssr.js")
	if err != nil {
		return nil, err
	}
	pool, err := jsruntime.New(ssr, 4)
	if err != nil {
		return nil, err
	}
	return webfront.New(webfront.Config{
		Mode:     webfront.ModeSSR,
		Renderer: pool,
		EmbedFS:  distFS,
		DistDir:  "web/dist",
	})
}
