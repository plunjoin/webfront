package dev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"github.com/plunjoin/webfront/compiler"
)

func TestDevServerImportMapAndDeps(t *testing.T) {
	tmp := t.TempDir()

	// 准备 .store/manifest.json
	storeDir := filepath.Join(tmp, "wf_modules", ".store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟 fake-dep
	pkgDir := filepath.Join(storeDir, "fake-dep@1.0.0")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"fake-dep","version":"1.0.0","module":"index.js"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "index.js"), []byte(`export const hello = "world";`), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := compiler.StoreManifest{
		Root: map[string]string{
			"fake-dep": "1.0.0",
		},
		Packages: map[string]map[string]string{},
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(storeDir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	handler := ds.Handler()

	// 1. 测试 GET / 返回 HTML 并包含 importmap
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<script type="importmap">`) {
		t.Fatalf("expected importmap tag in HTML: %s", body)
	}
	if !strings.Contains(body, `"/@deps/fake-dep.js"`) {
		t.Fatalf("expected /@deps/fake-dep.js in importmap: %s", body)
	}

	// 2. 测试 GET /@deps/fake-dep.js 成功打包并返回 ESM
	reqDep := httptest.NewRequest(http.MethodGet, "/@deps/fake-dep.js", nil)
	wDep := httptest.NewRecorder()
	handler.ServeHTTP(wDep, reqDep)
	if wDep.Code != http.StatusOK {
		t.Fatalf("expected 200 for /@deps/fake-dep.js, got %d: %s", wDep.Code, wDep.Body.String())
	}
	depBody := wDep.Body.String()
	if !strings.Contains(depBody, `hello`) {
		t.Fatalf("expected bundled code in /@deps/fake-dep.js, got: %s", depBody)
	}

	// 3. 再次请求测试缓存命中
	wDep2 := httptest.NewRecorder()
	handler.ServeHTTP(wDep2, reqDep)
	if wDep2.Code != http.StatusOK {
		t.Fatalf("expected 200 on cached dep, got %d", wDep2.Code)
	}
}
