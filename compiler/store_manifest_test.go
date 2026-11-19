package compiler

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSharedStoreProjectManifests：两个工程共用一个 store 时，各自的 root
// 依赖表互不覆盖；manifest.json 仍保留所有工程的并集给旧读取方。
func TestSharedStoreProjectManifests(t *testing.T) {
	tmp := t.TempDir()
	store := filepath.Join(tmp, "wf_modules", ".store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	portal := filepath.Join(tmp, "portal")
	admin := filepath.Join(tmp, "admin")

	portalManifest := StoreManifest{Root: map[string]string{"react": "18.3.1"}}
	adminManifest := StoreManifest{Root: map[string]string{"@shared/ui": "1.0.0"}}

	if err := SaveProjectStoreManifest(store, portal, portalManifest); err != nil {
		t.Fatal(err)
	}
	if err := SaveProjectStoreManifest(store, admin, adminManifest); err != nil {
		t.Fatal(err)
	}

	// 各自只看到自己的 root 依赖。
	got, err := LoadProjectStoreManifest(store, portal)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root["react"] != "18.3.1" || len(got.Root) != 1 {
		t.Fatalf("portal manifest = %#v", got.Root)
	}
	if _, ok := got.Root["@shared/ui"]; ok {
		t.Fatalf("portal 不应看到 admin 的依赖: %#v", got.Root)
	}

	got, err = LoadProjectStoreManifest(store, admin)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root["@shared/ui"] != "1.0.0" || len(got.Root) != 1 {
		t.Fatalf("admin manifest = %#v", got.Root)
	}

	// 合并版保留并集（旧读取方 / 人工排查）。
	merged, err := LoadStoreManifest(store)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Root["react"] != "18.3.1" || merged.Root["@shared/ui"] != "1.0.0" {
		t.Fatalf("merged manifest = %#v", merged.Root)
	}

	// 重新 install portal 不会影响 admin。
	if err := SaveProjectStoreManifest(store, portal, portalManifest); err != nil {
		t.Fatal(err)
	}
	got, err = LoadProjectStoreManifest(store, admin)
	if err != nil || got.Root["@shared/ui"] != "1.0.0" {
		t.Fatalf("admin manifest 被覆盖了: %#v, %v", got.Root, err)
	}

	// 没 install 过的工程回退到合并版（兼容旧布局）。
	other, err := LoadProjectStoreManifest(store, filepath.Join(tmp, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Root["react"] != "18.3.1" || other.Root["@shared/ui"] != "1.0.0" {
		t.Fatalf("未登记工程应回退到合并版: %#v", other.Root)
	}
}

// TestSharedStoreVersionConflict：同一个包在两个工程里版本不同时，
// 合并版只能保留一个，但每个工程读自己的 manifest 仍然正确。
func TestSharedStoreVersionConflict(t *testing.T) {
	tmp := t.TempDir()
	store := filepath.Join(tmp, "wf_modules", ".store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	if err := SaveProjectStoreManifest(store, a, StoreManifest{Root: map[string]string{"react": "18.3.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProjectStoreManifest(store, b, StoreManifest{Root: map[string]string{"react": "17.0.2"}}); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{a: "18.3.1", b: "17.0.2"} {
		got, err := LoadProjectStoreManifest(store, dir)
		if err != nil {
			t.Fatal(err)
		}
		if got.Root["react"] != want {
			t.Errorf("%s 读到 react@%s，期望 %s", dir, got.Root["react"], want)
		}
	}
}
