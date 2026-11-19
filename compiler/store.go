package compiler

// store.go：版本化依赖 store（pnpm 式）的解析器。
// 目标：同一构建里让不同包各用各的依赖版本（如 a→react@17、b→react@18）。
//
// 目录布局：
//   <storeDir>/<name>@<version>/...        （scoped 包名里的 "/" 换成 "+"）
//   <storeDir>/manifests/<key>.json        （每个工程自己的 root 依赖表）
//   <storeDir>/manifest.json               （所有工程合并版，兼容旧读取方）
//
// 解析规则：看"发起 import 的文件属于哪个包"，再查该包在 manifest 里声明的版本。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

// LoadStoreManifest 读取 <storeDir>/manifest.json（所有项目合并后的版本）。
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

// ProjectManifestKey 生成项目在共享 store 里的 manifest 文件名。
//
// 用"目录名 + 绝对路径哈希"而不是纯目录名：两个不同仓库里的 admin/ 不能撞车，
// 同一项目换工作目录也不会变（绝对路径变了会变，重新 install 一次即可）。
func ProjectManifestKey(projectDir string) string {
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		abs = projectDir
	}
	abs = filepath.Clean(abs)
	sum := sha256.Sum256([]byte(filepath.ToSlash(abs)))
	base := sanitizeKey(filepath.Base(abs))
	if base == "" || base == "." {
		base = "project"
	}
	return base + "-" + hex.EncodeToString(sum[:4])
}

func sanitizeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func projectManifestPath(storeDir, projectDir string) string {
	return filepath.Join(storeDir, "manifests", ProjectManifestKey(projectDir)+".json")
}

// LoadProjectStoreManifest 优先读本项目的 manifest（共享 store 场景下每个工程
// 各有一份），不存在时回退到合并版 manifest.json，兼容旧布局与手写测试数据。
func LoadProjectStoreManifest(storeDir, projectDir string) (StoreManifest, error) {
	if b, err := os.ReadFile(projectManifestPath(storeDir, projectDir)); err == nil {
		var m StoreManifest
		if json.Unmarshal(b, &m) == nil {
			return m, nil
		}
	}
	return LoadStoreManifest(storeDir)
}

// SaveProjectStoreManifest 写本项目专属 manifest，并把 store 里所有项目的
// manifest 合并回 manifest.json。
//
// 共享 store 时，先后 install 两个工程不会再互相覆盖：各自读
// manifests/<key>.json；合并版只是给旧版读取方（以及人工排查）看的并集。
// 同一依赖出现不同版本时保留先读到的版本并给出提示。
func SaveProjectStoreManifest(storeDir, projectDir string, m StoreManifest) error {
	dir := filepath.Join(storeDir, "manifests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(projectManifestPath(storeDir, projectDir), append(mb, '\n'), 0o644); err != nil {
		return err
	}

	merged := StoreManifest{Root: map[string]string{}, Packages: map[string]map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var pm StoreManifest
		if json.Unmarshal(b, &pm) != nil {
			continue
		}
		for k, v := range pm.Root {
			if old, ok := merged.Root[k]; ok && old != v {
				fmt.Printf("⚠️ 共享 store: %s 版本冲突（%s / %s），本项目以 manifests/%s 为准\n",
					k, old, v, ProjectManifestKey(projectDir)+".json")
				continue
			}
			merged.Root[k] = v
		}
		for pkg, deps := range pm.Packages {
			if merged.Packages[pkg] == nil {
				merged.Packages[pkg] = map[string]string{}
			}
			for k, v := range deps {
				merged.Packages[pkg][k] = v
			}
		}
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(storeDir, "manifest.json"), append(out, '\n'), 0o644)
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
	m, err := LoadProjectStoreManifest(storeDir, rootDir)
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
	m, err := LoadProjectStoreManifest(storeDir, rootDir)
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

// resolveSubpath 在包目录内解析子路径：
//  1. 包 package.json 的 exports（精确 / 通配符）
//  2. 当作文件补扩展名
//  3. 当作目录：目录自带 package.json 时按它的 exports/module/main 解析
//     （如 react-remove-scroll-bar/constants 里的 {"module": "../dist/es2015/constants.js"}），
//     否则回退 index 文件
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
	if resolved := resolveFileWithExt(base); resolved != "" {
		return resolved
	}
	// 子目录自带 package.json：按它自己的入口字段解析（目标可指向包内其他目录，
	// filepath.Join 会自然清理 ".."）。packageEntry 已包含 exports → module/main → index.js。
	if fi, err := os.Stat(base); err == nil && fi.IsDir() {
		if _, ok := readPackageJSON(base); ok {
			if entry := packageEntry(base); entry != "" {
				return entry
			}
		}
	}
	return ""
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
