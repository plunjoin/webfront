package adapter

import (
	"reflect"
	"testing"
)

type fakeAdapter struct {
	name     string
	exts     []string
	accept   bool
	kind     string
	accepted []string
}

func (f fakeAdapter) Name() string         { return f.name }
func (f fakeAdapter) Extensions() []string { return f.exts }
func (f fakeAdapter) SelfAccepts(id string) bool {
	for _, want := range f.accepted {
		if id == want {
			return true
		}
	}
	return f.accept
}
func (f fakeAdapter) HMRKind(string) string { return f.kind }

func TestRegistryExtensionAndSelfAccept(t *testing.T) {
	r := New(
		fakeAdapter{name: "x", exts: []string{"X", ".x"}, accept: true, kind: "x"},
		fakeAdapter{name: "y", exts: []string{".y"}, accepted: []string{"src/only.y"}, kind: "y"},
	)

	if got, want := r.Extensions(), []string{".x", ".y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Extensions() = %v, want %v", got, want)
	}
	if !r.SelfAccepts("src/a.X") {
		t.Fatal(".X 应命中大小写归一后的 .x 适配器")
	}
	if r.SelfAccepts("src/other.y") {
		t.Fatal("adapter 明确拒绝的 id 不应被判定为 self-accept")
	}
	if !r.SelfAccepts("src/only.y?raw") {
		t.Fatal("query 应被忽略后再匹配 adapter")
	}
	if got := r.HMRKind("src/a.x"); got != "x" {
		t.Fatalf("HMRKind = %q", got)
	}
	if got := r.HMRKind("src/a.css"); got != "" {
		t.Fatalf("未注册扩展名 HMRKind = %q, want empty", got)
	}
	if !r.Has("x") || r.Has("nope") {
		t.Fatalf("Has 结果不对: %v", r.Names())
	}
	if got, want := r.Names(), []string{"x", "y"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestRegistryNilSafety(t *testing.T) {
	var r *Registry
	if r.SelfAccepts("a.vue") || r.HMRKind("a.vue") != "" || r.Extensions() != nil ||
		r.Adapters() != nil || r.Names() != nil {
		t.Fatal("nil Registry 应返回零值且不 panic")
	}
}

func TestRegistryLastAdapterWins(t *testing.T) {
	first := fakeAdapter{name: "first", exts: []string{".z"}, accept: true, kind: "first"}
	second := fakeAdapter{name: "second", exts: []string{".z"}, accept: false, kind: "second"}
	r := New(first, second)
	if r.SelfAccepts("a.z") {
		t.Fatal("同扩展名应由后注册的 adapter 决定")
	}
	if got := r.HMRKind("a.z"); got != "second" {
		t.Fatalf("HMRKind = %q, want second", got)
	}
	if len(r.Adapters()) != 2 {
		t.Fatalf("Adapters() 应保留注册顺序与数量: %v", r.Adapters())
	}
}
