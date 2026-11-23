//go:build dev

package dev

import (
	"net/http"
	"strings"
	"testing"

	"github.com/plunjoin/webfront/module"
)

func hasGraphEdge(imports []module.Import, resolved, kind string) bool {
	for _, imp := range imports {
		if imp.Resolved == resolved && imp.Kind == kind {
			return true
		}
	}
	return false
}

func TestDevCSSImportsAndURLs(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/main.ts", `import "./styles/index.css"`)
	writeGraphFile(t, tmp, "src/styles/index.css", `@import "./base.css";
.a { background: url("./img/a.png"); }
.data { background: url("data:image/png;base64,AAAA"); }
`)
	writeGraphFile(t, tmp, "src/styles/base.css", `@import "./nested.css";
.b { color: red; }
`)
	writeGraphFile(t, tmp, "src/styles/nested.css", `.c { background: url("../img/b.png?x=1#frag"); }`)
	writeGraphFile(t, tmp, "src/styles/img/a.png", "A")
	writeGraphFile(t, tmp, "src/img/b.png", "B")

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := ds.Handler()

	index := doGet(h, "/src/styles/index.css")
	if index.Code != http.StatusOK {
		t.Fatalf("index.css = %d %s", index.Code, index.Body.String())
	}
	body := index.Body.String()
	for _, want := range []string{
		`import "/src/styles/base.css";`,
		`/@wf/asset/src/styles/img/a.png`,
		`data:image/png;base64,AAAA`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index.css 缺少 %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `@import "./base.css"`) {
		t.Fatalf("顶层 @import 应已转成 JS import:\n%s", body)
	}
	node, ok := ds.Module("src/styles/index.css")
	if !ok || !hasGraphEdge(node.Imports, "src/styles/base.css", "import-rule") {
		t.Fatalf("index.css 缺少 base.css 图边: %+v, ok=%v", node.Imports, ok)
	}

	base := doGet(h, "/src/styles/base.css")
	if !strings.Contains(base.Body.String(), `import "/src/styles/nested.css";`) {
		t.Fatalf("base.css 未转换嵌套 @import:\n%s", base.Body.String())
	}
	nested := doGet(h, "/src/styles/nested.css")
	if !strings.Contains(nested.Body.String(), `/@wf/asset/src/img/b.png?x=1#frag`) {
		t.Fatalf("nested.css url() 未改写:\n%s", nested.Body.String())
	}

	asset := doGet(h, "/@wf/asset/src/styles/img/a.png")
	if asset.Code != http.StatusOK || asset.Body.String() != "A" {
		t.Fatalf("asset = %d %q", asset.Code, asset.Body.String())
	}
}

func TestDevCSSVersionedImport(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/styles/index.css", `@import "./base.css"; .a{}`)
	writeGraphFile(t, tmp, "src/styles/base.css", `.b{}`)

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	ds.hmrMu.Lock()
	ds.dirty[module.Key("src/styles/base.css")] = 9
	ds.hmrMu.Unlock()

	body := doGet(ds.Handler(), "/src/styles/index.css").Body.String()
	if !strings.Contains(body, `import "/src/styles/base.css?t=9";`) {
		t.Fatalf("脏 CSS 依赖应带 ?t=9:\n%s", body)
	}
}

func TestDevCSSBasePath(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/styles/index.css", `@import "./base.css"; .a{background:url("./a.png")}`)
	writeGraphFile(t, tmp, "src/styles/base.css", `.b{}`)
	writeGraphFile(t, tmp, "src/styles/a.png", "A")

	ds, err := NewDevServer(tmp, WithBasePath("/admin"))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	body := doGet(ds.Handler(), "/admin/src/styles/index.css").Body.String()
	for _, want := range []string{
		`import "/admin/src/styles/base.css";`,
		`/admin/@wf/asset/src/styles/a.png`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("basePath 产物缺少 %q:\n%s", want, body)
		}
	}
}

func TestDevCSSConditionalImport(t *testing.T) {
	tmp := t.TempDir()
	writeGraphFile(t, tmp, "src/styles/print.css", `@import "./base.css" print; .p{}`)
	writeGraphFile(t, tmp, "src/styles/base.css", `.b{color:red}`)

	ds, err := NewDevServer(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	h := ds.Handler()

	body := doGet(h, "/src/styles/print.css").Body.String()
	want := `@import url(\"/@wf/asset/src/styles/base.css\") print;`
	if !strings.Contains(body, want) {
		t.Fatalf("条件 @import 应保留条件并指向原始 CSS，want %q:\n%s", want, body)
	}
	if node, ok := ds.Module("src/styles/print.css"); ok {
		if hasGraphEdge(node.Imports, "src/styles/base.css", "import-rule") ||
			hasGraphEdge(node.Imports, "src/styles/base.css", "url-token") {
			t.Fatalf("条件 @import 不应作为模块图边: %+v", node.Imports)
		}
	}
	raw := doGet(h, "/@wf/asset/src/styles/base.css")
	if raw.Code != http.StatusOK || !strings.Contains(raw.Body.String(), ".b{color:red}") {
		t.Fatalf("原始 CSS = %d %q", raw.Code, raw.Body.String())
	}
}

// CSS @import 现在是独立模块；被依赖的样式热更新时必须原位替换，
// remove + append 会把它移到 head 末尾，改变级联顺序。
func TestCSSModuleReplacesStyleInPlace(t *testing.T) {
	js := string(cssModuleJS(`.b{}`, "src/base.css", "/src/base.css", "/@wf/client", true))
	if !strings.Contains(js, "old.parentNode.replaceChild(s,old)") {
		t.Fatalf("样式热更新应原位替换:\n%s", js)
	}
	if strings.Contains(js, "old.remove()") {
		t.Fatalf("样式热更新不应先移除再追加:\n%s", js)
	}
}
