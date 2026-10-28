package compiler

// engine.go：把"构建"和"运行"缝在一起的一层便利 API。
// 生产部署不需要它（直接 webfront.New + 嵌入 ssr.js），开发/单机自编译时用它。

import (
	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/jsruntime"
)

// New 先构建 SSR bundle，再起一台带 JS 引擎的引擎（动态 SSR）。
//
// vmPoolSize 是 JS 引擎实例池大小（<=0 取默认 2）。它放在这一层而不是
// webfront.Config 里，因为只有这里才真正创建引擎池 —— 核心包不 import jsruntime，
// 也就无从决定池大小（早先放在 Config 里是个误导性的死字段）。
func New(cfg webfront.Config, vmPoolSize int) (*webfront.Engine, error) {
	entry := cfg.ServerEntry
	if entry == "" {
		entry = "src/entry-server.tsx"
	}
	b, err := BuildSSRWith(cfg.RootDir, entry, cfg.ModulesDir)
	if err != nil {
		return nil, err
	}
	if vmPoolSize <= 0 {
		vmPoolSize = 2
	}
	pool, err := jsruntime.New(b, vmPoolSize)
	if err != nil {
		return nil, err
	}
	cfg.Renderer = pool
	return webfront.New(cfg)
}
