package module

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCanonicalID(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "src", "App.tsx")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, ok := CanonicalID(root, file)
	if !ok || id != "src/App.tsx" {
		t.Fatalf("CanonicalID = %q, %v", id, ok)
	}
	// 不存在的路径（删除事件）也要能算出 ID。
	gone := filepath.Join(root, "src", "Gone.tsx")
	if id, ok := CanonicalID(root, gone); !ok || id != "src/Gone.tsx" {
		t.Fatalf("deleted CanonicalID = %q, %v", id, ok)
	}
	if _, ok := CanonicalID(root, filepath.Join(filepath.Dir(root), "outside.ts")); ok {
		t.Fatal("项目外路径不应有 canonical ID")
	}
	if _, ok := CanonicalID(root, root); ok {
		t.Fatal("项目根目录本身不应是模块")
	}
}

func TestKeyCase(t *testing.T) {
	if runtime.GOOS == "windows" {
		if Key("src/App.tsx") != Key("SRC/app.TSX") {
			t.Fatal("Windows 上模块键应大小写不敏感")
		}
		return
	}
	if Key("src/App.tsx") == Key("src/app.tsx") {
		t.Fatal("非 Windows 上大小写是模块身份的一部分")
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Clean("/tmp/wf-root")
	if _, ok := Within(root, filepath.Join(root, "src", "a.ts")); !ok {
		t.Fatal("项目内路径应命中")
	}
	if _, ok := Within(root, filepath.Join(root, "..", "a.ts")); ok {
		t.Fatal("项目外路径不应命中")
	}
	if _, ok := Within(root, root); !ok {
		t.Fatal("根目录本身应命中 Within")
	}
	if !strings.HasPrefix(filepath.ToSlash(root), "/") {
		t.Skip("非 POSIX 路径布局，跳过")
	}
}
