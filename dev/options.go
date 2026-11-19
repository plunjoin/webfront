package dev

// options.go：DevServer 与 dev.Handler 的可选配置。
//
// 零配置仍是默认形态（NewDevServer("./web")），下面的选项只解决"要挂到别处"
// 的场景：子路径部署、共享依赖目录、框架 runtime 单例、自定义 define。
//
// 本文件不带 build tag：生产构建也会编译它，所以这里不能 import esbuild /
// fsnotify 等只有开发态才需要的包。DevServer 专属的接线在 options_dev.go。

import (
	"os"
	"path/filepath"
	"strings"
)

// Option 修改 dev server 的默认行为。在 NewDevServer 探测依赖之前生效。
//
// 生产构建（没有 -tags dev）里 dev.Handler 不会使用选项：它们只影响
// -tags dev 构建出来的 DevServer。
type Option func(*config)

// config 是 Option 的载体。它不带 build tag，两种构建都能编译。
type config struct {
	basePath    string
	modulesDir  string
	define      map[string]string
	externals   map[string]bool
	externalSet bool
	publicDir   string
	publicSet   bool
}

// newConfig 依次应用选项。nil 选项直接跳过，方便调用方按条件构造。
func newConfig(opts []Option) *config {
	c := &config{}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithBasePath 让 dev server 整体挂在前缀下（如 "/admin"）。
//
// 生成的 index.html、import map、HMR 广播与 WebSocket 地址都会带上该前缀，
// 调用方按普通 mux 挂载即可：
//
//	mux.Handle("/admin/", ds.Handler())
//
// 传入 "/" 或空串等价于不挂前缀；末尾斜杠会被去掉。
func WithBasePath(p string) Option {
	return func(c *config) {
		c.basePath = normalizeBasePath(p)
	}
}

// WithModulesDir 显式指定依赖目录（相对 root 或绝对路径）。
//
// 默认按 <root>/wf_modules → <root>/node_modules → 逐级向上的 wf_modules
// 探测（与 CLI 的 -modules auto 同一套规则）。
func WithModulesDir(dir string) Option {
	return func(c *config) {
		c.modulesDir = dir
	}
}

// WithPublicDir 指定开发态静态资源目录（默认 <root>/public）。
//
// 未命中内部路由的请求会回退到该目录（Vite 的 publicDir 约定）：目录内的文件
// 按 URL 原样提供，仍找不到才回 SPA 外壳。目录可相对 root 或为绝对路径；
// 传空串关闭 public 目录托管。与 wf build 的约定一致：构建期复制、开发期直出。
func WithPublicDir(dir string) Option {
	return func(c *config) {
		c.publicSet = true
		c.publicDir = dir
	}
}

// WithDefine 追加 esbuild 的 define 表（key 是源码里的全局名，value 是 JS 字面量源码）。
//
//	dev.WithDefine(map[string]string{"__APP_VERSION__": `"1.2.3"`})
//
// 默认已含 process.env.NODE_ENV=development，检测到 Vue 依赖时再补
// __VUE_OPTIONS_API__ / __VUE_PROD_DEVTOOLS__ 等特性开关。
func WithDefine(m map[string]string) Option {
	return func(c *config) {
		if c.define == nil {
			c.define = map[string]string{}
		}
		for k, v := range m {
			c.define[k] = v
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
	return func(c *config) {
		c.externalSet = true
		c.externals = map[string]bool{}
		for _, s := range specs {
			if pkg := packageOf(strings.TrimSpace(s)); pkg != "" {
				c.externals[pkg] = true
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
