package webfront_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plunjoin/webfront"
	"github.com/plunjoin/webfront/compiler"
	"github.com/plunjoin/webfront/jsruntime"
)

// TestTemplateE2E 钉死"React/Vue 当 Go 模板引擎"这条路径：
// 真实 esbuild 构建 entry-server → goja 执行 render(name, props) → Engine 拼完整 HTML。
// 这是 template 模式与 RenderTemplate 的契约测试，不用假 Renderer。
func TestTemplateE2E(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "src", "entry-server.ts")
	src := `export function render(name: string, props: any) {
  return { html: '<div id="root">' + name + ':' + (props.title ?? '') + '</div>', head: '<title>' + name + '</title>' }
}`
	if err := os.WriteFile(entry, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := compiler.BuildSSRWith(dir, entry, "node_modules")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := jsruntime.New(bundle, 1)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := webfront.New(webfront.Config{Mode: webfront.ModeSSR, Renderer: pool, ClientScript: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := eng.RenderTemplateTo(w, req, "Home", struct {
		Title string `json:"title"`
	}{Title: "首页"}); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, body)
	}
	if !strings.Contains(body, "Home:首页") {
		t.Fatalf("goja 没执行模板: %s", body)
	}
	if !strings.Contains(body, "<title>Home</title>") {
		t.Fatalf("head 没拼进去: %s", body)
	}
}
