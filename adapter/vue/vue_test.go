package vue

import (
	"reflect"
	"testing"

	"github.com/plunjoin/webfront/adapter"
)

func TestAdapter(t *testing.T) {
	a := Adapter{}
	if a.Name() != "vue" {
		t.Fatalf("Name = %q", a.Name())
	}
	if got, want := a.Extensions(), []string{".vue"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Extensions = %v, want %v", got, want)
	}
	for _, id := range []string{"src/App.vue", "src/App.VUE", "src/App.vue?t=3"} {
		if !a.SelfAccepts(id) {
			t.Errorf("SelfAccepts(%q) = false", id)
		}
	}
	for _, id := range []string{"src/App.tsx", "src/vue.ts", "src/.vuex"} {
		if a.SelfAccepts(id) {
			t.Errorf("SelfAccepts(%q) = true", id)
		}
	}
	if got := a.HMRKind("src/App.vue"); got != "vue" {
		t.Fatalf("HMRKind = %q", got)
	}

	r := adapter.New(a)
	if !r.SelfAccepts("src/App.vue") || !r.Has("vue") {
		t.Fatal("Vue adapter 未注册成功")
	}
}
