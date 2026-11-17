package compiler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func aliasMap(aliases []Alias) map[string]string {
	m := make(map[string]string, len(aliases))
	for _, a := range aliases {
		m[a.Prefix] = a.Target
	}
	return m
}

// 默认约定：@/ → src/，且只有 src/ 真实存在时才添加。
func TestLoadAliasesDefaultAtSrc(t *testing.T) {
	tmp := t.TempDir()
	if got := LoadAliases(tmp); len(got) != 0 {
		t.Fatalf("没有 src/ 时不应产生别名: %+v", got)
	}

	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliases := LoadAliases(tmp)
	m := aliasMap(aliases)
	want := filepath.Join(tmp, "src")
	if m["@/"] != want {
		t.Fatalf("@/ = %q，期望 %q（全部: %+v）", m["@/"], want, aliases)
	}
}

// tsconfig 的 paths/baseUrl（JSONC 注释与尾逗号）→ 别名表。
func TestLoadAliasesFromTSConfig(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  // 常用别名
  "compilerOptions": {
    "baseUrl": ".",
    /* 前缀映射 + 精确映射 */
    "paths": {
      "@ui/*": ["packages/ui/src/*"],
      "@config": ["./src/config.ts"],
      "@external/*": ["../shared/*"],
    },
  },
}`)

	m := aliasMap(LoadAliases(tmp))
	cases := map[string]string{
		"@ui/":       filepath.Join(tmp, "packages", "ui", "src"),
		"@config":    filepath.Join(tmp, "src", "config.ts"),
		"@external/": filepath.Join(filepath.Dir(tmp), "shared"),
	}
	for k, want := range cases {
		if m[k] != want {
			t.Errorf("%s = %q，期望 %q（全部: %+v）", k, m[k], want, m)
		}
	}
}

// 显式 paths 覆盖默认 @/；没有 baseUrl 时目标相对 tsconfig 所在目录（TS 5 行为）。
func TestLoadAliasesExplicitOverridesDefault(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  "compilerOptions": { "paths": { "@/*": ["./app/*"] } }
}`)

	m := aliasMap(LoadAliases(tmp))
	if want := filepath.Join(tmp, "app"); m["@/"] != want {
		t.Fatalf("@/ = %q，期望显式配置的 %q", m["@/"], want)
	}
}

// extends 链与 project references（tsconfig.app.json）里的 paths 都要生效。
func TestLoadAliasesExtendsAndReferences(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "tsconfig.base.json"), `{
  "compilerOptions": { "baseUrl": ".", "paths": { "@base/*": ["./base/*"] } }
}`)
	writeTestFile(t, filepath.Join(tmp, "tsconfig.base2.json"), `{
  "compilerOptions": { "baseUrl": ".", "paths": { "@base2/*": ["./base2/*"] } }
}`)
	writeTestFile(t, filepath.Join(tmp, "tsconfig.app.json"), `{
  "compilerOptions": { "baseUrl": ".", "paths": { "@app/*": ["./app/*"] } }
}`)
	// TS 5 允许 extends 是数组。
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  "extends": ["./tsconfig.base", "./tsconfig.base2.json"],
  "references": [{ "path": "./tsconfig.app.json" }]
}`)

	m := aliasMap(LoadAliases(tmp))
	if want := filepath.Join(tmp, "base"); m["@base/"] != want {
		t.Errorf("@base/ = %q，期望 %q", m["@base/"], want)
	}
	if want := filepath.Join(tmp, "base2"); m["@base2/"] != want {
		t.Errorf("@base2/ = %q，期望 %q", m["@base2/"], want)
	}
	if want := filepath.Join(tmp, "app"); m["@app/"] != want {
		t.Errorf("@app/ = %q，期望 %q", m["@app/"], want)
	}
}

func TestResolveAliasLongestPrefix(t *testing.T) {
	aliases := []Alias{
		{Prefix: "@/", Target: "/root/src"},
		{Prefix: "@ui/", Target: "/root/ui"},
		{Prefix: "@config", Target: "/root/config.ts"},
	}
	// LoadAliases 会排序；这里显式按最长前缀排，验证匹配逻辑。
	aliases[0], aliases[1] = aliases[1], aliases[0]

	cases := []struct {
		spec string
		want string
		ok   bool
	}{
		{"@ui/Button", filepath.Join("/root/ui", "Button"), true},
		{"@/lib/x", filepath.Join("/root/src", "lib", "x"), true},
		{"@config", "/root/config.ts", true},
		{"@config/extra", "", false},
		{"react", "", false},
	}
	for _, c := range cases {
		got, ok := ResolveAlias(aliases, c.spec)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ResolveAlias(%q) = (%q, %v)，期望 (%q, %v)", c.spec, got, ok, c.want, c.ok)
		}
	}
}

// 构建链路：没有 tsconfig 时，默认 @/ 也必须能解析。
func TestBuildSSRDefaultAlias(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "src", "lib", "greet.js"),
		`export const greet = "hello-from-alias";`)
	writeTestFile(t, filepath.Join(tmp, "src", "entry.js"),
		`import { greet } from "@/lib/greet"; globalThis.__RESULT__ = greet;`)

	out, err := BuildSSRWith(tmp, "src/entry.js", "node_modules")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if !strings.Contains(string(out), "hello-from-alias") {
		t.Fatalf("产物里没有别名目标的内容:\n%s", out)
	}
}

// 构建链路：tsconfig paths 与 dev 使用同一套解析；没有 baseUrl 时相对 tsconfig 目录。
func TestBuildSSRUsesTSConfigPaths(t *testing.T) {
	tmp := t.TempDir()
	writeTestFile(t, filepath.Join(tmp, "tsconfig.json"), `{
  "compilerOptions": { "paths": { "@/*": ["./app/*"] } }
}`)
	writeTestFile(t, filepath.Join(tmp, "app", "util.ts"),
		`export const util = "from-tsconfig-paths";`)
	writeTestFile(t, filepath.Join(tmp, "src", "entry.js"),
		`import { util } from "@/util"; globalThis.__RESULT__ = util;`)

	out, err := BuildSSRWith(tmp, "src/entry.js", "node_modules")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if !strings.Contains(string(out), "from-tsconfig-paths") {
		t.Fatalf("产物里没有 tsconfig paths 目标的内容:\n%s", out)
	}
}
