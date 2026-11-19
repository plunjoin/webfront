package dev

import (
	"strings"
	"testing"
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
