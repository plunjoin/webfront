package compiler

// store.go：版本化依赖 store（pnpm 式）的解析器。
// 目标：同一构建里让不同包各用各的依赖版本（如 a→react@17、b→react@18）。
//
// 目录布局：
//   <storeDir>/<name>@<version>/...        （scoped 包名里的 "/" 换成 "+"）
//   <storeDir>/manifest.json               （root 依赖 + 每个包的依赖版本表）
//
// 解析规则：看"发起 import 的文件属于哪个包"，再查该包在 manifest 里声明的版本。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// StoreManifest：版本化 store 的解析清单。
//
//	Root:     app 自身的依赖 name -> version
//	Packages: "name@version" -> { depName -> version }
type StoreManifest struct {
	Root     map[string]string            `json:"root"`
	Packages map[string]map[string]string `json:"packages"`
}

// StoreDirName 把包名+版本转成 store 里的目录名（scoped 的 "/" → "+"）。
func StoreDirName(name, version string) string {
	return strings.ReplaceAll(name, "/", "+") + "@" + version
}

// LoadStoreManifest 读取 <storeDir>/manifest.json。
func LoadStoreManifest(storeDir string) (StoreManifest, error) {
	b, err := os.ReadFile(filepath.Join(storeDir, "manifest.json"))
	if err != nil {
		return StoreManifest{}, err
	}
	var m StoreManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return StoreManifest{}, err
	}
	return m, nil
}

// resolveModulesDir 统一解析依赖目录路径（支持相对 rootDir 路径或绝对路径）。
func resolveModulesDir(rootDir, modulesDir string) string {
	if modulesDir == "" {
		modulesDir = "node_modules"
	}
	if filepath.IsAbs(modulesDir) {
		return modulesDir
	}
	return filepath.Join(rootDir, modulesDir)
}

// StorePackageDir 返回某个包在 store 布局下的真实目录（按 root 声明的版本）。
// 用于构建期需要"直接读包内文件"的场景（如 @vue/compiler-sfc 的 package.json）。
// store 不存在或未声明该依赖时返回 ""，调用方应回退平铺路径。
func StorePackageDir(rootDir, modulesDir, pkgName string) string {
	storeDir := filepath.Join(resolveModulesDir(rootDir, modulesDir), ".store")
	m, err := LoadStoreManifest(storeDir)
	if err != nil {
		return ""
	}
	ver, ok := m.Root[pkgName]
	if !ok {
		return ""
	}
	dir := filepath.Join(storeDir, StoreDirName(pkgName, ver))
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return ""
	}
	return dir
}

// StorePlugins 若 <rootDir>/<modulesDir>/.store/manifest.json 存在，返回 store 解析插件。
func StorePlugins(rootDir, modulesDir string) []api.Plugin {
	return storePlugins(rootDir, modulesDir)
}

// storePlugins 若 <rootDir>/<modulesDir>/.store/manifest.json 存在，返回 store 解析插件。
func storePlugins(rootDir, modulesDir string) []api.Plugin {
	storeDir := filepath.Join(resolveModulesDir(rootDir, modulesDir), ".store")
	m, err := LoadStoreManifest(storeDir)
	if err != nil {
		return nil
	}
	return []api.Plugin{StoreResolver(storeDir, m)}
}

// StoreResolver 返回 esbuild 插件：按 importer 所属包决定依赖版本，
// 从版本化 store 解析裸导入（含子路径，如 react-dom/server.browser）。
func StoreResolver(storeDir string, m StoreManifest) api.Plugin {
	absStore, _ := filepath.Abs(storeDir)
	return api.Plugin{
		Name: "webfront-store",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				p := args.Path
				// 相对/绝对路径交回默认解析
				if strings.HasPrefix(p, ".") || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
					return api.OnResolveResult{}, nil
				}
				// 裸 specifier 的入口点（如 loadCompilerSFC 的 "@vue/compiler-sfc"）也走 store；
				// 相对入口点已在上面返回。

				// 1) 拆包名 + 子路径
				pkgName, sub := splitSpecifier(p)
				// 2) importer 属于哪个包？
				owner := ownerPackage(absStore, args.Importer)
				var deps map[string]string
				if owner == "" {
					deps = m.Root
				} else {
					deps = m.Packages[owner]
				}
				ver, ok := deps[pkgName]
				if !ok {
					if v, ok2 := m.Root[pkgName]; ok2 { // 提升回退
						ver, ok = v, true
					}
				}
				if !ok {
					return api.OnResolveResult{}, nil
				}
				// 3) 定位入口文件（根导出或子路径）
				dir := filepath.Join(absStore, StoreDirName(pkgName, ver))
				var entry string
				if sub == "" {
					entry = packageEntry(dir)
				} else {
					entry = resolveSubpath(dir, sub)
				}
				if entry == "" {
					return api.OnResolveResult{}, nil
				}
				return api.OnResolveResult{Path: entry}, nil
			})
		},
	}
}

// splitSpecifier 拆出包名与子路径：
//
//	"react"                 -> ("react", "")
//	"react-dom/server.browser" -> ("react-dom", "server.browser")
//	"@vue/compiler-dom"     -> ("@vue/compiler-dom", "")
//	"@vue/compiler-sfc/foo" -> ("@vue/compiler-sfc", "foo")
func splitSpecifier(p string) (pkg, sub string) {
	if strings.HasPrefix(p, "@") {
		parts := strings.SplitN(p, "/", 3)
		if len(parts) >= 2 {
			pkg = parts[0] + "/" + parts[1]
			if len(parts) == 3 {
				sub = parts[2]
			}
			return pkg, sub
		}
		return p, ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// resolveFileWithExt 检查路径是否存在，若不存在则尝试补扩展名与 index 文件。
func resolveFileWithExt(base string) string {
	if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
		return base
	}
	for _, e := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".json"} {
		if fi, err := os.Stat(base + e); err == nil && !fi.IsDir() {
			return base + e
		}
	}
	for _, e := range []string{".js", ".jsx", ".mjs", ".cjs", ".json"} {
		p := filepath.Join(base, "index"+e)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

type packageJSON struct {
	Exports any    `json:"exports"`
	Module  string `json:"module"`
	Main    string `json:"main"`
}

func readPackageJSON(dir string) (packageJSON, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return packageJSON{}, false
	}
	var pkg packageJSON
	if err := json.Unmarshal(b, &pkg); err != nil {
		return packageJSON{}, false
	}
	return pkg, true
}

func resolveExportTarget(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if res := resolveExportTarget(item); res != "" {
				return res
			}
		}
	case map[string]any:
		// 条件导出优先级：import > browser > module > default > node > require
		for _, cond := range []string{"import", "browser", "module", "default", "node", "require"} {
			if subVal, ok := v[cond]; ok {
				if res := resolveExportTarget(subVal); res != "" {
					return res
				}
			}
		}
		for k, subVal := range v {
			if !strings.HasPrefix(k, ".") {
				if res := resolveExportTarget(subVal); res != "" {
					return res
				}
			}
		}
	}
	return ""
}

func resolveExportRoot(exports any) string {
	switch v := exports.(type) {
	case string:
		return v
	case []any:
		return resolveExportTarget(v)
	case map[string]any:
		if dotVal, ok := v["."]; ok {
			return resolveExportTarget(dotVal)
		}
		// 若没有任何以 "." 开头的 key，则为顶层条件导出（如 {"import": "...", "default": "..."}）
		hasDotKeys := false
		for k := range v {
			if strings.HasPrefix(k, ".") {
				hasDotKeys = true
				break
			}
		}
		if !hasDotKeys {
			return resolveExportTarget(v)
		}
	}
	return ""
}

func resolveExportSubpath(exports any, sub string) string {
	expMap, ok := exports.(map[string]any)
	if !ok {
		return ""
	}
	subKey := "./" + filepath.ToSlash(strings.TrimPrefix(sub, "./"))

	// 1. 精确匹配
	if val, ok := expMap[subKey]; ok {
		return resolveExportTarget(val)
	}

	// 2. 通配符模式匹配（如 "./*" -> "./dist/*"）
	for pattern, val := range expMap {
		if !strings.Contains(pattern, "*") || !strings.HasPrefix(pattern, "./") {
			continue
		}
		prefix, suffix, found := strings.Cut(pattern, "*")
		if !found {
			continue
		}
		if strings.HasPrefix(subKey, prefix) && strings.HasSuffix(subKey, suffix) && len(subKey) >= len(prefix)+len(suffix) {
			matched := subKey[len(prefix) : len(subKey)-len(suffix)]
			targetPattern := resolveExportTarget(val)
			if targetPattern != "" {
				return strings.Replace(targetPattern, "*", matched, 1)
			}
		}
	}
	return ""
}

// resolveSubpath 在包目录内解析子路径（优先支持 package.json 的 exports，回退补扩展名 / index 文件）。
func resolveSubpath(dir, sub string) string {
	if pkg, ok := readPackageJSON(dir); ok && pkg.Exports != nil {
		if target := resolveExportSubpath(pkg.Exports, sub); target != "" {
			full := filepath.Join(dir, filepath.FromSlash(target))
			if resolved := resolveFileWithExt(full); resolved != "" {
				return resolved
			}
		}
	}
	base := filepath.Join(dir, filepath.FromSlash(sub))
	return resolveFileWithExt(base)
}

// ownerPackage 从 importer 的绝对路径反推 store 里的 "<name>@<version>"。
// importer 不在 store 内（即 app 自身代码）时返回 ""。
func ownerPackage(absStore, importer string) string {
	if importer == "" {
		return ""
	}
	rel, err := filepath.Rel(absStore, importer)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 2 && strings.Contains(parts[0], "@") {
		return parts[0]
	}
	return ""
}

// packageEntry 读包 package.json 的 exports/module/main，回退 index.js。
func packageEntry(dir string) string {
	if pkg, ok := readPackageJSON(dir); ok {
		// 1. 优先读取 exports
		if pkg.Exports != nil {
			if target := resolveExportRoot(pkg.Exports); target != "" {
				full := filepath.Join(dir, filepath.FromSlash(target))
				if resolved := resolveFileWithExt(full); resolved != "" {
					return resolved
				}
			}
		}
		// 2. 回退 module / main
		for _, f := range []string{pkg.Module, pkg.Main} {
			if f == "" {
				continue
			}
			full := filepath.Join(dir, filepath.FromSlash(f))
			if resolved := resolveFileWithExt(full); resolved != "" {
				return resolved
			}
		}
	}
	idx := filepath.Join(dir, "index.js")
	if fi, err := os.Stat(idx); err == nil && !fi.IsDir() {
		return idx
	}
	return ""
}
