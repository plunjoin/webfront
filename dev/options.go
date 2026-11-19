package dev

// options.go：DevServer 的可选配置。
//
// 零配置仍是默认形态（NewDevServer("./web")），下面的选项只解决"要挂到别处"
// 的场景：子路径部署、共享依赖目录、框架 runtime 单例、自定义 define。

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Option 修改 DevServer 的默认行为。在 NewDevServer 探测依赖之前生效。
type Option func(*DevServer)

// WithBasePath 让 dev server 整体挂在前缀下（如 "/admin"）。
//
// 生成的 index.html、import map、HMR 广播与 WebSocket 地址都会带上该前缀，
// 调用方按普通 mux 挂载即可：
//
//	mux.Handle("/admin/", ds.Handler())
//
// 传入 "/" 或空串等价于不挂前缀；末尾斜杠会被去掉。
func WithBasePath(p string) Option {
	return func(d *DevServer) {
		d.basePath = normalizeBasePath(p)
	}
}

// WithModulesDir 显式指定依赖目录（相对 root 或绝对路径）。
//
// 默认按 <root>/wf_modules → <root>/node_modules → 逐级向上的 wf_modules
// 探测（与 CLI 的 -modules auto 同一套规则）。
func WithModulesDir(dir string) Option {
	return func(d *DevServer) {
		d.modulesDir = dir
	}
}

// WithDefine 追加 esbuild 的 define 表（key 是源码里的全局名，value 是 JS 字面量源码）。
//
//	dev.WithDefine(map[string]string{"__APP_VERSION__": `"1.2.3"`})
//
// 默认已含 process.env.NODE_ENV=development，检测到 Vue 依赖时再补
// __VUE_OPTIONS_API__ / __VUE_PROD_DEVTOOLS__ 等特性开关。
func WithDefine(m map[string]string) Option {
	return func(d *DevServer) {
		if d.userDefine == nil {
			d.userDefine = map[string]string{}
		}
		for k, v := range m {
			d.userDefine[k] = v
		}
	}
}

// WithExternal 指定 /@deps 打包时必须外置的包（按包名匹配，含子路径）。
//
// 外置的裸导入会原样留在产物里，由 import map 路由到同一个 /@deps/<pkg>.js，
// 从而保证 react / react-dom、vue / @vue/runtime-* 这类 runtime 只有一份实例。
//
// 不调用该选项时，默认把所有 root 依赖（manifest.Root / package.json
// dependencies）外置，效果与 Vite 的 optimizeDeps 单例一致；传空参数则关闭自动外置。
func WithExternal(specs ...string) Option {
	return func(d *DevServer) {
		d.externalSet = true
		d.externals = map[string]bool{}
		for _, s := range specs {
			if pkg := packageOf(strings.TrimSpace(s)); pkg != "" {
				d.externals[pkg] = true
			}
		}
	}
}

// normalizeBasePath 统一成""（无前缀）或"/admin"这种形式。
func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	return p
}

// url 给 dev server 自己的路径加挂载前缀。
func (d *DevServer) url(p string) string {
	return d.basePath + p
}

// packageOf 取裸 specifier 的包名（"@scope/pkg/sub" → "@scope/pkg"）。
func packageOf(spec string) string {
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || filepath.IsAbs(spec) {
		return ""
	}
	if strings.HasPrefix(spec, "@") {
		parts := strings.SplitN(spec, "/", 3)
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return ""
	}
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i]
	}
	return spec
}

// findModulesDir 探测依赖目录，顺序与 CLI 的 resolveModules 一致：
//
//  1. <root>/wf_modules、<root>/node_modules 里已有 manifest.json 的那个；
//  2. 工程内已存在且有内容的依赖目录（只有空的 .store 视为中断安装，跳过）；
//  3. 逐级向上找共享的 <parent>/wf_modules（前端工程把 store 放在上级目录）；
//  4. 都没有时返回 "node_modules"。
//
// 向上只认 wf_modules：node_modules 可能在任意祖先目录里，误认一个无关的
// 目录比找不到更糟。
func findModulesDir(root string) (string, bool) {
	for _, md := range []string{"wf_modules", "node_modules"} {
		if hasStoreManifest(filepath.Join(root, md)) {
			return md, true
		}
	}
	for _, md := range []string{"wf_modules", "node_modules"} {
		if usableModulesDir(filepath.Join(root, md)) {
			return md, true
		}
	}
	dir := root
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		cand := filepath.Join(parent, "wf_modules")
		if hasStoreManifest(cand) {
			return cand, true
		}
		dir = parent
	}
	return "", false
}

func hasStoreManifest(modulesDir string) bool {
	fi, err := os.Stat(filepath.Join(modulesDir, ".store", "manifest.json"))
	return err == nil && !fi.IsDir()
}

// usableModulesDir：目录存在，且除了 .store 之外还有内容。
func usableModulesDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != ".store" {
			return true
		}
	}
	return false
}

// buildDefine 合并默认 define、Vue 特性开关与调用方自定义项（后者优先）。
//
// Vue 3 的 runtime 把 __VUE_OPTIONS_API__ 等当成裸全局读取，构建期不注入
// 就会在浏览器里 ReferenceError。Vite 同样会自动补这几个开关。
func buildDefine(rootDeps, user map[string]string) map[string]string {
	def := map[string]string{
		"process.env.NODE_ENV": "\"development\"",
	}
	if hasVue(rootDeps) {
		def["__VUE_OPTIONS_API__"] = "true"
		def["__VUE_PROD_DEVTOOLS__"] = "false"
		def["__VUE_PROD_HYDRATION_MISMATCH_DETAILS__"] = "false"
	}
	for k, v := range user {
		def[k] = v
	}
	return def
}

func hasVue(rootDeps map[string]string) bool {
	for pkg := range rootDeps {
		if pkg == "vue" || strings.HasPrefix(pkg, "@vue/") {
			return true
		}
	}
	return false
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
