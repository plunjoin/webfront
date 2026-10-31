package compiler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// DetectServerEntry 在 webDir 下按约定探测 SSR 入口，返回相对路径（找不到返回 ""）。
// 覆盖 entry-server.tsx/.ts/.jsx/.js 以及 Vue 常用的 entry-server.vue.tsx 写法。
func DetectServerEntry(webDir string) string {
	return detectEntry(webDir, []string{
		"src/entry-server.tsx", "src/entry-server.ts", "src/entry-server.jsx", "src/entry-server.js",
		"src/entry-server.vue.tsx", "src/entry-server.vue.ts", "src/entry-server.vue.jsx", "src/entry-server.vue.js",
	})
}

// DetectClientEntry 探测客户端入口：优先 entry-client.*，回退 main.*。
func DetectClientEntry(webDir string) string {
	return detectEntry(webDir, []string{
		"src/entry-client.tsx", "src/entry-client.ts", "src/entry-client.jsx", "src/entry-client.js",
		"src/main.tsx", "src/main.ts", "src/main.jsx", "src/main.js",
	})
}

func detectEntry(webDir string, candidates []string) string {
	for _, p := range candidates {
		if fi, err := os.Stat(filepath.Join(webDir, filepath.FromSlash(p))); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// modulesNodePaths：当依赖目录不叫 node_modules 时，把它作为 esbuild 的 NodePaths
// （等价 NODE_PATH 语义），从而在"自定义依赖目录名"下也能解析包。
func modulesNodePaths(rootDir, modulesDir string) []string {
	if modulesDir == "" || modulesDir == "node_modules" {
		return nil
	}
	abs, err := filepath.Abs(resolveModulesDir(rootDir, modulesDir))
	if err != nil {
		return nil
	}
	return []string{abs}
}

// BuildSSR bundles the SSR entry (e.g. src/entry-server.tsx) into a single
// IIFE that exposes globalThis.__WF_APP__ = { render }. Pure-Go, no Node.
func BuildSSR(rootDir, entry string) ([]byte, error) {
	return BuildSSRWith(rootDir, entry, "node_modules")
}

// BuildSSRWith 同 BuildSSR，但可指定依赖目录名（默认 node_modules）。
func BuildSSRWith(rootDir, entry, modulesDir string) ([]byte, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	res := api.Build(api.BuildOptions{
		Bundle:        true,
		Write:         false,
		Format:        api.FormatIIFE,
		GlobalName:    "__WF_APP__",
		Platform:      api.PlatformBrowser,
		EntryPoints:   []string{entry},
		AbsWorkingDir: abs,
		NodePaths:     modulesNodePaths(rootDir, modulesDir),
		Plugins:       append(storePlugins(rootDir, modulesDir), VuePlugin(rootDir, modulesDir)),
		Define:        map[string]string{"process.env.NODE_ENV": "\"production\""},
		// SSR 产物跑在我们的 JS 引擎里，不是浏览器：统一降级到 ES2017，
		// 让 async generator / 逻辑赋值等新语法由 esbuild 编译掉，
		// 引擎（尤其 goja）就不用追新语法。客户端产物不降级。
		Target:   api.ES2017,
		LogLevel: api.LogLevelError,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("esbuild SSR build: %+v", res.Errors)
	}
	return res.OutputFiles[0].Contents, nil
}

// BuildClient bundles the client entry (hydration bootstrap) to an IIFE that
// self-executes on load (no module type needed). Pure-Go, no Node.
func BuildClient(rootDir, entry, outfile string) ([]byte, error) {
	return BuildClientWith(rootDir, entry, outfile, "node_modules")
}

// BuildClientWith 同 BuildClient，但可指定依赖目录名（默认 node_modules）。
// 客户端产物直接进浏览器，故默认开启压缩（whitespace+identifiers+syntax）：
// React 18 未压缩 252KB → 压缩后 139KB；再叠加 StaticHandler 的 gzip 后线上约 45KB。
func BuildClientWith(rootDir, entry, outfile, modulesDir string) ([]byte, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	res := api.Build(api.BuildOptions{
		Bundle:            true,
		Write:             false,
		Format:            api.FormatIIFE,
		Platform:          api.PlatformBrowser,
		EntryPoints:       []string{entry},
		AbsWorkingDir:     abs,
		Outfile:           outfile,
		NodePaths:         modulesNodePaths(rootDir, modulesDir),
		Plugins:           append(storePlugins(rootDir, modulesDir), VuePlugin(rootDir, modulesDir)),
		Define:            map[string]string{"process.env.NODE_ENV": "\"production\""},
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		LogLevel:          api.LogLevelError,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("esbuild client build: %+v", res.Errors)
	}
	return res.OutputFiles[0].Contents, nil
}

// ---------------------------------------------------------------------------
// 代码分割（Vite 式 chunk 拆分）
//
// esbuild 的 Splitting 只在 ESM 下生效，且没有 manualChunks，只按"模块被几个入口共享"
// 自动切分。于是这里用两遍构建复刻 Vite 的 vendor chunk：
//   1) 先普通构建一次，从 metafile 里读出"客户端真正用到的裸包"（避免把
//      @vue/server-renderer 这类 server-only 依赖塞进 vendor）；
//   2) 再以这些包生成一个虚拟 vendor 入口，与真实入口一起构建 → 共享依赖被提升成
//      content-hash 的 chunk，业务代码留在 client.js 里。
// 收益：改业务代码时 vendor chunk 的 hash 不变，浏览器缓存全命中。
// ---------------------------------------------------------------------------

// BuildClientSplit 产出多个文件：入口 client.js + chunks/<name>-<hash>.js。
// outDir 为相对 AbsWorkingDir 的输出目录（如 "dist/assets"）。
func BuildClientSplit(rootDir, entry, outDir, modulesDir string) ([]api.OutputFile, error) {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	vendor := clientUsedPackages(abs, entry, modulesDir)
	// 只保留"根依赖"：传递依赖（如 scheduler）会被 react-dom 自己带进来，
	// 写进虚拟入口反而会因不在 manifest.Root 里而解析失败。
	vendor = filterRootDeps(abs, modulesDir, vendor)

	plugins := append(storePlugins(rootDir, modulesDir), VuePlugin(rootDir, modulesDir))
	plugins = append(plugins, virtualVendorPlugin(abs, vendor))

	res := api.Build(api.BuildOptions{
		Bundle:        true,
		Write:         false,
		Splitting:     true,
		Format:        api.FormatESModule, // Splitting 只支持 ESM
		Platform:      api.PlatformBrowser,
		AbsWorkingDir: abs,
		Outdir:        outDir,
		// 入口产物固定叫 client.js（esbuild 默认用入口文件名，这里统一改名，
		// 好处是 SSR 注入的 <script src="/assets/client.js"> 不用跟着变）。
		EntryPoints:       []string{entry, "wf:vendor"}, // 第二个入口只为把共享依赖逼成独立 chunk
		EntryNames:        "[name]",
		ChunkNames:        "chunks/[name]-[hash]",
		AssetNames:        "assets/[name]-[hash]",
		NodePaths:         modulesNodePaths(rootDir, modulesDir),
		Plugins:           plugins,
		Define:            map[string]string{"process.env.NODE_ENV": "\"production\""},
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		LogLevel:          api.LogLevelError,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("esbuild split build: %+v", res.Errors)
	}
	// vendor.js 本身没人加载（它只是"锚点"），不落盘；入口产物统一改名：
	// entry-client.js → client.js、entry-client.css → client.css，
	// 这样"入口叫什么"不影响 SSR 注入的 <script>/<link> 路径约定。
	entryBase := strings.TrimSuffix(filepath.Base(entry), filepath.Ext(entry))
	out := make([]api.OutputFile, 0, len(res.OutputFiles))
	for _, f := range res.OutputFiles {
		base := filepath.Base(f.Path)
		if base == "vendor.js" || base == "wf_vendor.js" {
			continue
		}
		ext := filepath.Ext(base)
		if strings.TrimSuffix(base, ext) == entryBase {
			f.Path = filepath.Join(filepath.Dir(f.Path), "client"+ext)
		}
		out = append(out, f)
	}
	return out, nil
}

// virtualVendorPlugin 提供虚拟入口 "wf:vendor"：把客户端用到的根依赖重新导出一次。
func virtualVendorPlugin(resolveDir string, pkgs []string) api.Plugin {
	return api.Plugin{
		Name: "wf-virtual-vendor",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: "^wf:vendor$"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: "vendor", Namespace: "wf-virtual"}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "wf-virtual"}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				var sb strings.Builder
				for _, p := range pkgs {
					fmt.Fprintf(&sb, "import %q;\n", p)
				}
				src := sb.String()
				// ResolveDir 让 esbuild 知道"从哪个目录出发找包"，否则裸导入无法回退到默认解析。
				return api.OnLoadResult{Contents: &src, Loader: api.LoaderJS, ResolveDir: resolveDir}, nil
			})
		},
	}
}

// filterRootDeps 只保留 store manifest 里登记为根依赖的包；无 store 时原样返回。
func filterRootDeps(abs, modulesDir string, pkgs []string) []string {
	storeDir := filepath.Join(resolveModulesDir(abs, modulesDir), ".store")
	m, err := LoadStoreManifest(storeDir)
	if err != nil || len(m.Root) == 0 {
		return pkgs
	}
	out := pkgs[:0]
	for _, p := range pkgs {
		if _, ok := m.Root[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// clientUsedPackages 用一次普通构建 + metafile 反推"客户端真实用到的包名"。
func clientUsedPackages(abs, entry, modulesDir string) []string {
	res := api.Build(api.BuildOptions{
		Bundle:        true,
		Write:         false,
		Format:        api.FormatIIFE,
		Platform:      api.PlatformBrowser,
		EntryPoints:   []string{entry},
		AbsWorkingDir: abs,
		// 入口若 import 了 CSS，一次构建会有 JS+CSS 两个输出，esbuild 要求显式
		// outdir/outfile 才肯出多文件（Write:false 不落盘，这里只是占位）。
		Outdir:    "wf-probe",
		NodePaths: modulesNodePaths(abs, modulesDir),
		Plugins:   append(storePlugins(abs, modulesDir), VuePlugin(abs, modulesDir)),
		Define:    map[string]string{"process.env.NODE_ENV": "\"production\""},
		Metafile:  true,
		LogLevel:  api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		return nil // 探路失败就退化成"不拆 vendor"
	}
	var meta struct {
		Outputs map[string]struct {
			Inputs map[string]struct{} `json:"inputs"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(res.Metafile), &meta); err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, out := range meta.Outputs {
		for in := range out.Inputs {
			if p := pkgFromStorePath(in); p != "" {
				set[p] = true
			}
		}
	}
	list := make([]string, 0, len(set))
	for p := range set {
		list = append(list, p)
	}
	sort.Strings(list)
	return list
}

// pkgFromStorePath 从 store 内路径反推包名：
//
//	<md>/.store/@vue+runtime-dom@3.5.13/dist/x.js -> "@vue/runtime-dom"
//	<md>/.store/react@18.3.1/cjs/x.js            -> "react"
func pkgFromStorePath(p string) string {
	p = filepath.ToSlash(p)
	const marker = "/.store/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	rest := p[i+len(marker):]
	dir := rest
	if j := strings.Index(rest, "/"); j >= 0 {
		dir = rest[:j]
	}
	if k := strings.LastIndex(dir, "@"); k > 0 {
		dir = dir[:k]
	}
	return strings.ReplaceAll(dir, "+", "/")
}
