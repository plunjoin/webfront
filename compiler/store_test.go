package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageEntryAndSubpathExports(t *testing.T) {
	tmp := t.TempDir()

	// 1. 测试包：modern-pkg (包含多种 exports)
	pkgDir := filepath.Join(tmp, "modern-pkg")
	if err := os.MkdirAll(filepath.Join(pkgDir, "dist", "features"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 写入实体文件
	if err := os.WriteFile(filepath.Join(pkgDir, "dist", "index.mjs"), []byte("export const a = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "dist", "server.mjs"), []byte("export const server = true;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "dist", "features", "auth.mjs"), []byte("export const auth = true;"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgJSON := `{
		"name": "modern-pkg",
		"exports": {
			".": {
				"import": "./dist/index.mjs",
				"default": "./dist/index.cjs"
			},
			"./server": {
				"import": "./dist/server.mjs"
			},
			"./features/*": {
				"import": "./dist/features/*.mjs"
			}
		},
		"module": "./dist/legacy.js",
		"main": "./dist/legacy.js"
	}`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// 验证根导出优先走 exports["."]
	entry := packageEntry(pkgDir)
	expectedEntry := filepath.Join(pkgDir, "dist", "index.mjs")
	if filepath.Clean(entry) != filepath.Clean(expectedEntry) {
		t.Fatalf("expected entry %s, got %s", expectedEntry, entry)
	}

	// 验证子路径导出 ./server
	subServer := resolveSubpath(pkgDir, "server")
	expectedSub := filepath.Join(pkgDir, "dist", "server.mjs")
	if filepath.Clean(subServer) != filepath.Clean(expectedSub) {
		t.Fatalf("expected subpath %s, got %s", expectedSub, subServer)
	}

	// 验证通配符子路径 ./features/auth
	subAuth := resolveSubpath(pkgDir, "features/auth")
	expectedAuth := filepath.Join(pkgDir, "dist", "features", "auth.mjs")
	if filepath.Clean(subAuth) != filepath.Clean(expectedAuth) {
		t.Fatalf("expected wildcard subpath %s, got %s", expectedAuth, subAuth)
	}

	// 2. 测试仅有顶层条件导出的包 (如 {"import": "./dist/index.mjs"})
	pkgDir2 := filepath.Join(tmp, "top-level-cond")
	if err := os.MkdirAll(filepath.Join(pkgDir2, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir2, "dist", "index.mjs"), []byte("export const b = 2;"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgJSON2 := `{
		"name": "top-level-cond",
		"exports": {
			"import": "./dist/index.mjs",
			"default": "./dist/index.cjs"
		}
	}`
	if err := os.WriteFile(filepath.Join(pkgDir2, "package.json"), []byte(pkgJSON2), 0o644); err != nil {
		t.Fatal(err)
	}
	entry2 := packageEntry(pkgDir2)
	expectedEntry2 := filepath.Join(pkgDir2, "dist", "index.mjs")
	if filepath.Clean(entry2) != filepath.Clean(expectedEntry2) {
		t.Fatalf("expected entry2 %s, got %s", expectedEntry2, entry2)
	}
}

// TestResolveSubpathWithNestedPackageJSON 覆盖 react-remove-scroll-bar/constants 这类
// "子目录自带 package.json（指向包内其他目录）" 的包。
func TestResolveSubpathWithNestedPackageJSON(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "react-remove-scroll-bar")

	for _, dir := range []string{
		filepath.Join(pkgDir, "dist", "es5"),
		filepath.Join(pkgDir, "dist", "es2015"),
		filepath.Join(pkgDir, "constants"),
		filepath.Join(pkgDir, "legacy"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pkgDir, "package.json"),
		`{"name":"react-remove-scroll-bar","main":"dist/es5/index.js","module":"dist/es2015/index.js"}`)
	write(filepath.Join(pkgDir, "dist", "es5", "constants.js"), `export var K = "es5";`)
	write(filepath.Join(pkgDir, "dist", "es2015", "constants.js"), `export var K = "es2015";`)
	write(filepath.Join(pkgDir, "constants", "package.json"),
		`{"private":true,"main":"../dist/es5/constants.js","module":"../dist/es2015/constants.js"}`)
	// 没有 package.json 的普通目录，仍回退 index.js
	write(filepath.Join(pkgDir, "legacy", "index.js"), `export var K = "legacy";`)

	got := resolveSubpath(pkgDir, "constants")
	want := filepath.Join(pkgDir, "dist", "es2015", "constants.js")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("nested package.json subpath: expected %s, got %s", want, got)
	}

	gotLegacy := resolveSubpath(pkgDir, "legacy")
	wantLegacy := filepath.Join(pkgDir, "legacy", "index.js")
	if filepath.Clean(gotLegacy) != filepath.Clean(wantLegacy) {
		t.Fatalf("plain directory index: expected %s, got %s", wantLegacy, gotLegacy)
	}
}

// TestStoreResolverNestedPackageJSON 走完整 esbuild 解析链路，确认插件能解析
// "包名/子目录"（子目录自带 package.json）形式的导入。
func TestStoreResolverNestedPackageJSON(t *testing.T) {
	tmp := t.TempDir()
	appDir := filepath.Join(tmp, "app")
	storeDir := filepath.Join(tmp, "wf_modules", ".store")
	pkgDir := filepath.Join(storeDir, "react-remove-scroll-bar@2.3.8")

	for _, dir := range []string{
		filepath.Join(appDir, "src"),
		filepath.Join(pkgDir, "dist", "es5"),
		filepath.Join(pkgDir, "dist", "es2015"),
		filepath.Join(pkgDir, "constants"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pkgDir, "package.json"),
		`{"name":"react-remove-scroll-bar","version":"2.3.8","main":"dist/es5/index.js","module":"dist/es2015/index.js"}`)
	write(filepath.Join(pkgDir, "dist", "es5", "constants.js"), `export var K = "es5-constants";`)
	write(filepath.Join(pkgDir, "dist", "es2015", "constants.js"), `export var K = "es2015-constants";`)
	write(filepath.Join(pkgDir, "constants", "package.json"),
		`{"private":true,"main":"../dist/es5/constants.js","module":"../dist/es2015/constants.js"}`)
	write(filepath.Join(storeDir, "manifest.json"),
		`{"root":{"react-remove-scroll-bar":"2.3.8"},"packages":{}}`)
	write(filepath.Join(appDir, "src", "entry.js"),
		`import { K } from 'react-remove-scroll-bar/constants'; console.log(K);`)

	out, err := BuildSSRWith(appDir, "src/entry.js", "../wf_modules")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if !strings.Contains(string(out), "es2015-constants") {
		t.Fatalf("bundle should come from nested package.json module field, got:\n%s", out)
	}
}
