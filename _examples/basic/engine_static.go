//go:build !dynamic

// 默认构建：预渲染静态模式 —— 不链接 JS 引擎。
package main

import (
	"github.com/plunjoin/webfront"
)

const modeDesc = "预渲染静态 · 无 JS 引擎"

func newEngine() (*webfront.Engine, error) {
	return webfront.New(webfront.Config{
		Mode:    webfront.ModeSSR,
		EmbedFS: distFS,
		DistDir: "web/dist",
	})
}
