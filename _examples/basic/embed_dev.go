//go:build dev

package main

import "embed"

// dev 构建不读产物：dev.Handler 返回 DevServer，prodHandler 只被编译、
// 不会被调用。空 FS 让生产 handler 照常编译，也让 `go build -tags dev`
// 不要求 web/dist 已经构建。
var distFS embed.FS
