package compiler

import (
	"os"
	"path/filepath"
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
