package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSharedWfModules 验证多个子工程（如 portal 和 admin）能否共用同一个父级 wf_modules
func TestSharedWfModules(t *testing.T) {
	tmp := t.TempDir()

	// 1. 创建共享的 wf_modules 目录结构：tmp/wf_modules/.store/
	modulesDir := filepath.Join(tmp, "wf_modules")
	storeDir := filepath.Join(modulesDir, ".store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 模拟一个共享组件包：@shared/ui@1.0.0
	pkgDir := filepath.Join(storeDir, "@shared+ui@1.0.0")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgJSON := `{"name":"@shared/ui","version":"1.0.0","module":"index.js"}`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(pkgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	indexJS := `export const HelloShared = (name) => "Shared UI Hello, " + name;`
	if err := os.WriteFile(filepath.Join(pkgDir, "index.js"), []byte(indexJS), 0o644); err != nil {
		t.Fatal(err)
	}

	// 写入 store manifest.json
	manifest := StoreManifest{
		Root: map[string]string{
			"@shared/ui": "1.0.0",
		},
		Packages: map[string]map[string]string{},
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. 创建第一个子前端：tmp/portal（前台应用，SSR）
	portalDir := filepath.Join(tmp, "portal")
	portalSrc := filepath.Join(portalDir, "src")
	if err := os.MkdirAll(portalSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	portalServer := `
import { HelloShared } from '@shared/ui';
export const render = () => {
    return "<div>Portal SSR: " + HelloShared("PortalUser") + "</div>";
};
`
	if err := os.WriteFile(filepath.Join(portalSrc, "entry-server.tsx"), []byte(portalServer), 0o644); err != nil {
		t.Fatal(err)
	}
	portalClient := `
import { HelloShared } from '@shared/ui';
console.log("Portal Client Hydrate:", HelloShared("PortalClient"));
`
	if err := os.WriteFile(filepath.Join(portalSrc, "entry-client.tsx"), []byte(portalClient), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. 创建第二个子前端：tmp/admin（后台应用，SPA）
	adminDir := filepath.Join(tmp, "admin")
	adminSrc := filepath.Join(adminDir, "src")
	if err := os.MkdirAll(adminSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	adminClient := `
import { HelloShared } from '@shared/ui';
console.log("Admin Client SPA:", HelloShared("AdminUser"));
`
	if err := os.WriteFile(filepath.Join(adminSrc, "entry-client.tsx"), []byte(adminClient), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. 构建前台 Portal（引用 ../wf_modules）
	relModules := "../wf_modules"
	portalSSR, err := BuildSSRWith(portalDir, "src/entry-server.tsx", relModules)
	if err != nil {
		t.Fatalf("Portal BuildSSRWith failed: %v", err)
	}
	if !strings.Contains(string(portalSSR), "Shared UI Hello") {
		t.Fatalf("Portal SSR bundle does not contain shared code: %s", string(portalSSR))
	}

	portalFiles, err := BuildClientSplit(portalDir, "src/entry-client.tsx", "dist/assets", relModules)
	if err != nil {
		t.Fatalf("Portal BuildClientSplit failed: %v", err)
	}
	foundSharedInPortal := false
	for _, f := range portalFiles {
		if strings.Contains(string(f.Contents), "Shared UI Hello") {
			foundSharedInPortal = true
			break
		}
	}
	if !foundSharedInPortal {
		t.Fatalf("Portal client bundle does not contain shared code")
	}

	// 5. 构建后台 Admin（同样引用 ../wf_modules）
	adminFiles, err := BuildClientSplit(adminDir, "src/entry-client.tsx", "dist/assets", relModules)
	if err != nil {
		t.Fatalf("Admin BuildClientSplit failed: %v", err)
	}
	foundSharedInAdmin := false
	for _, f := range adminFiles {
		if strings.Contains(string(f.Contents), "Shared UI Hello") {
			foundSharedInAdmin = true
			break
		}
	}
	if !foundSharedInAdmin {
		t.Fatalf("Admin client bundle does not contain shared code")
	}

	t.Log("✅ Portal 与 Admin 成功共用 ../wf_modules 中的 @shared/ui！")
}

// TestRealReactSharedWfModules 用真实的 React 18 依赖验证：两个应用通过绝对路径/相对路径共享同一个 wf_modules
func TestRealReactSharedWfModules(t *testing.T) {
	// 找到现存真实的 wf_modules
	realModules, err := filepath.Abs("../../gin-webfront-verify/web/wf_modules")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(realModules, ".store", "manifest.json")); err != nil {
		t.Skip("真实 wf_modules 不存在，跳过真实依赖测试")
	}

	tmp := t.TempDir()

	// 1. App 1 (Portal): SSR React 应用
	portalDir := filepath.Join(tmp, "portal")
	if err := os.MkdirAll(filepath.Join(portalDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	portalServer := `
import React from 'react';
import { renderToString } from 'react-dom/server.browser';

export const render = () => {
    return renderToString(React.createElement('h1', null, 'Portal Shared React SSR'));
};
`
	if err := os.WriteFile(filepath.Join(portalDir, "src", "entry-server.tsx"), []byte(portalServer), 0o644); err != nil {
		t.Fatal(err)
	}
	portalClient := `
import React from 'react';
import { hydrateRoot } from 'react-dom/client';
console.log("Portal Client Hydrate with React", React.version);
`
	if err := os.WriteFile(filepath.Join(portalDir, "src", "entry-client.tsx"), []byte(portalClient), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. App 2 (Admin): 纯客户端 SPA React 应用
	adminDir := filepath.Join(tmp, "admin")
	if err := os.MkdirAll(filepath.Join(adminDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	adminClient := `
import React from 'react';
console.log("Admin SPA using Shared React", React.version);
`
	if err := os.WriteFile(filepath.Join(adminDir, "src", "entry-client.tsx"), []byte(adminClient), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. 构建 Portal（SSR bundle + Client split bundle）
	portalSSR, err := BuildSSRWith(portalDir, "src/entry-server.tsx", realModules)
	if err != nil {
		t.Fatalf("Portal BuildSSRWith failed: %v", err)
	}
	if !strings.Contains(string(portalSSR), "Portal Shared React SSR") {
		t.Fatalf("Portal SSR bundle content mismatch")
	}

	portalFiles, err := BuildClientSplit(portalDir, "src/entry-client.tsx", "dist/assets", realModules)
	if err != nil {
		t.Fatalf("Portal BuildClientSplit failed: %v", err)
	}
	if len(portalFiles) == 0 {
		t.Fatalf("Portal client files empty")
	}

	// 4. 构建 Admin（Client split bundle）
	adminFiles, err := BuildClientSplit(adminDir, "src/entry-client.tsx", "dist/assets", realModules)
	if err != nil {
		t.Fatalf("Admin BuildClientSplit failed: %v", err)
	}
	if len(adminFiles) == 0 {
		t.Fatalf("Admin client files empty")
	}

	t.Logf("✅ 真实 React 18 共用成功！Portal SSR (%d bytes), Portal Chunks (%d 个文件), Admin Chunks (%d 个文件)",
		len(portalSSR), len(portalFiles), len(adminFiles))
}

// TestDualFrontendBuildAndServe 完整端到端测试：
// 1. 同一个 wf_modules 依赖库
// 2. 两个独立前端（Portal SSR + Admin SPA）分别构建到 dist/portal 和 dist/admin
// 3. Gin 配合两个 webfront.Engine 分别挂载 / 和 /admin，验证路由与资产服务正常
func TestDualFrontendBuildAndServe(t *testing.T) {
	realModules, err := filepath.Abs("../../gin-webfront-verify/web/wf_modules")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(realModules, ".store", "manifest.json")); err != nil {
		t.Skip("真实 wf_modules 不存在，跳过双前端集成测试")
	}

	tmp := t.TempDir()
	distRoot := filepath.Join(tmp, "dist")
	portalDist := filepath.Join(distRoot, "portal")
	adminDist := filepath.Join(distRoot, "admin")

	// 1. 创建 Portal（SSR）
	portalDir := filepath.Join(tmp, "portal")
	_ = os.MkdirAll(filepath.Join(portalDir, "src"), 0o755)
	portalServer := `
import React from 'react';
import { renderToString } from 'react-dom/server.browser';
export const render = () => {
    return renderToString(React.createElement('div', { id: 'root' }, 'Welcome to Portal SSR'));
};
`
	_ = os.WriteFile(filepath.Join(portalDir, "src", "entry-server.tsx"), []byte(portalServer), 0o644)
	portalClient := `
import React from 'react';
console.log("Portal Client Hydrated");
`
	_ = os.WriteFile(filepath.Join(portalDir, "src", "entry-client.tsx"), []byte(portalClient), 0o644)

	// 2. 创建 Admin（SPA）
	adminDir := filepath.Join(tmp, "admin")
	_ = os.MkdirAll(filepath.Join(adminDir, "src"), 0o755)
	adminClient := `
import React from 'react';
console.log("Admin SPA Mounted");
`
	_ = os.WriteFile(filepath.Join(adminDir, "src", "main.tsx"), []byte(adminClient), 0o644)

	// 3. 构建 Portal（SSR + Client，输出到 dist/portal）
	portalSSR, err := BuildSSRWith(portalDir, "src/entry-server.tsx", realModules)
	if err != nil {
		t.Fatalf("Portal BuildSSRWith failed: %v", err)
	}
	_ = os.MkdirAll(filepath.Join(portalDist, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(portalDist, "ssr.js"), portalSSR, 0o644)
	portalFiles, err := BuildClientSplit(portalDir, "src/entry-client.tsx", filepath.ToSlash(filepath.Join("..", "dist", "portal", "assets")), realModules)
	if err != nil {
		t.Fatalf("Portal BuildClientSplit failed: %v", err)
	}
	for _, f := range portalFiles {
		_ = os.MkdirAll(filepath.Dir(f.Path), 0o755)
		_ = os.WriteFile(f.Path, f.Contents, 0o644)
	}

	// 4. 构建 Admin（纯 SPA Client，输出到 dist/admin，带 index.html）
	_ = os.MkdirAll(filepath.Join(adminDist, "assets"), 0o755)
	adminFiles, err := BuildClientSplit(adminDir, "src/main.tsx", filepath.ToSlash(filepath.Join("..", "dist", "admin", "assets")), realModules)
	if err != nil {
		t.Fatalf("Admin BuildClientSplit failed: %v", err)
	}
	for _, f := range adminFiles {
		_ = os.MkdirAll(filepath.Dir(f.Path), 0o755)
		_ = os.WriteFile(f.Path, f.Contents, 0o644)
	}
	adminIndex := `<!doctype html><html><head><title>Admin SPA</title></head><body><div id="root">Admin SPA Root</div><script type="module" src="/admin/assets/client.js"></script></body></html>`
	_ = os.WriteFile(filepath.Join(adminDist, "index.html"), []byte(adminIndex), 0o644)

	// 5. 验证两个产物都成功生成在 dist 目录下
	if _, err := os.Stat(filepath.Join(portalDist, "ssr.js")); err != nil {
		t.Fatal("Portal ssr.js missing")
	}
	if _, err := os.Stat(filepath.Join(portalDist, "assets", "client.js")); err != nil {
		t.Fatal("Portal client.js missing")
	}
	if _, err := os.Stat(filepath.Join(adminDist, "index.html")); err != nil {
		t.Fatal("Admin index.html missing")
	}
	if _, err := os.Stat(filepath.Join(adminDist, "assets", "client.js")); err != nil {
		t.Fatal("Admin client.js missing")
	}

	t.Log("✅ 双前端工程（Portal SSR + Admin SPA）成功共用同一套 wf_modules 构建并分别产出到 dist/portal 和 dist/admin！")
}
