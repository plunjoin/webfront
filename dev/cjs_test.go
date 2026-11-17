package dev

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/plunjoin/webfront/compiler"
)

// addCJSInterop 只该动 CJS 入口的产物：ESM 的具名导出原样保留，
// 没有可静态识别的导出时也不能瞎加。
func TestAddCJSInterop(t *testing.T) {
	cjs := `var require_impl = __commonJS({
  "pkg/impl.js"(exports, module) {
    exports.alpha = 1;
    exports["beta"] = 2;
    Object.defineProperty(exports, "__esModule", { value: true });
    Object.defineProperty(exports, "gamma", { get() { return 3; } });
  }
});
var require_pkg = __commonJS({
  "pkg/index.js"(exports, module) {
    module.exports = require_impl();
  }
});
export default require_pkg();
`
	got := string(addCJSInterop([]byte(cjs)))
	for _, want := range []string{
		"const __wf_cjs_dep = require_pkg();",
		"export default __wf_cjs_dep;",
		"export const alpha = __wf_cjs_dep.alpha;",
		"export const beta = __wf_cjs_dep.beta;",
		"export const gamma = __wf_cjs_dep.gamma;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "export const __esModule") {
		t.Errorf("__esModule 不是真实导出，不该被补出来:\n%s", got)
	}

	// ESM 依赖（vue 这类）不能被改。
	esm := "const version = \"1\";\nexport { version };\nexport default createApp();\n"
	if out := string(addCJSInterop([]byte(esm))); out != esm {
		t.Errorf("ESM 产物被改动了:\n%s", out)
	}

	// 入口没有任何可识别的导出：保持 default-only。
	bare := `var require_pkg = __commonJS({
  "pkg/index.js"(exports, module) {
    module.exports = function () {};
  }
});
export default require_pkg();
`
	if out := string(addCJSInterop([]byte(bare))); out != bare {
		t.Errorf("没有导出名时应原样返回:\n%s", out)
	}
}

// 端到端：/@deps 返回的 CJS 包要同时有 default 与具名导出，
// 并且产物仍然是浏览器能直接 import 的合法 ESM。
func TestDevDepsCJSNamedExports(t *testing.T) {
	tmp := t.TempDir()
	storeDir := filepath.Join(tmp, "wf_modules", ".store")
	pkgDir := filepath.Join(storeDir, "fake-cjs@1.0.0")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pkgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name":"fake-cjs","version":"1.0.0","main":"index.js"}`)
	write("index.js", `module.exports = require("./impl.js");`)
	write("impl.js", `exports.alpha = "A";
exports.beta = "B";
exports.useThing = function () {};
Object.defineProperty(exports, "__esModule", { value: true });`)

	manifest := compiler.StoreManifest{
		Root:     map[string]string{"fake-cjs": "1.0.0"},
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
	rec := httptest.NewRecorder()
	ds.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/@deps/fake-cjs.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /@deps/fake-cjs.js = %d: %s", rec.Code, rec.Body.String())
	}
	code := rec.Body.String()
	for _, want := range []string{
		"export default __wf_cjs_dep;",
		`export const alpha = __wf_cjs_dep.alpha;`,
		`export const beta = __wf_cjs_dep.beta;`,
		`export const useThing = __wf_cjs_dep.useThing;`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("产物缺少 %q:\n%s", want, code)
		}
	}

	// 产物必须能被当作 ESM 解析（浏览器模块图比 Transform 更严格，
	// 但语法错误在 Transform 阶段就会暴露）。
	if res := api.Transform(code, api.TransformOptions{Loader: api.LoaderJS, Format: api.FormatESModule}); len(res.Errors) > 0 {
		t.Fatalf("产物不是合法 ESM: %+v\n%s", res.Errors, code)
	}
}
