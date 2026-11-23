//go:build dev

package dev

// options_dev.go：只有 -tags dev 构建才需要的选项落地。
//
// 轻量的 Option / config 在 options.go（生产构建也会编译）；这里负责把
// config 搬到 DevServer 上，并放 esbuild 相关的插件。

import (
	"github.com/evanw/esbuild/pkg/api"

	"github.com/plunjoin/webfront/adapter"
	vueadapter "github.com/plunjoin/webfront/adapter/vue"
)

// applyConfig 把 Option 收集到的配置搬到 DevServer 上。
// 必须在依赖探测之前调用：modulesDir / externals 会影响探测结果。
func (d *DevServer) applyConfig(c *config) {
	d.basePath = c.basePath
	d.modulesDir = c.modulesDir
	d.publicDir = c.publicDir
	d.publicSet = c.publicSet
	d.userDefine = c.define
	if c.externalSet {
		d.externalSet = true
		d.externals = c.externals
	}
	// Vue 永远是默认适配器；调用方追加的适配器排在后面，同扩展名时胜出。
	// 只有这个装配点认识具体实现，DevServer 与 HMR 规划（hmr.go）只用接口。
	list := append([]adapter.Adapter{vueadapter.Adapter{}}, c.adapters...)
	d.adapters = adapter.New(list...)
}

// url 给 dev server 自己的路径加挂载前缀。
func (d *DevServer) url(p string) string {
	return d.basePath + p
}

// externalPlugin 把命中列表的裸导入标记为 external，交给浏览器的 import map
// 解析；entryPkg 自己的内部导入不外置，避免把同一个包拆成多份。
func externalPlugin(externals map[string]bool, entryPkg string) api.Plugin {
	return api.Plugin{
		Name: "webfront-dev-external",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: `^[^./]`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				// 入口点本身（importer 为空）必须打包，否则产物只剩一句自引用 import。
				if args.Importer == "" || args.Kind == api.ResolveEntryPoint {
					return api.OnResolveResult{}, nil
				}
				pkg := packageOf(args.Path)
				if pkg == "" || pkg == entryPkg || !externals[pkg] {
					return api.OnResolveResult{}, nil
				}
				return api.OnResolveResult{Path: args.Path, External: true}, nil
			})
		},
	}
}
