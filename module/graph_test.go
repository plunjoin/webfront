package module

import "testing"

func TestGraphImportsImportersAndVersion(t *testing.T) {
	g := NewGraph()
	app := g.Set(Node{
		ID:   "src/App.tsx",
		URL:  "/src/App.tsx",
		Kind: KindJS,
		Hash: "h1",
		Imports: []Import{
			{Specifier: "./Child", Resolved: "src/Child.tsx", Kind: "import-statement"},
			{Specifier: "vue", External: true, Kind: "import-statement"},
		},
	})
	if app.Version != 1 {
		t.Fatalf("first version = %d", app.Version)
	}
	if got := g.Importers("src/Child.tsx"); len(got) != 1 || got[0] != "src/App.tsx" {
		t.Fatalf("importers = %v", got)
	}

	// 内容不变：version 不变。
	app2 := g.Set(Node{ID: "src/App.tsx", URL: "/src/App.tsx", Kind: KindJS, Hash: "h1",
		Imports: []Import{{Specifier: "./Child", Resolved: "src/Child.tsx"}}})
	if app2.Version != app.Version {
		t.Fatalf("same hash bumped version: %d -> %d", app.Version, app2.Version)
	}
	// 内容变化：version +1。
	app3 := g.Set(Node{ID: "src/App.tsx", URL: "/src/App.tsx", Kind: KindJS, Hash: "h2"})
	if app3.Version != app.Version+1 {
		t.Fatalf("new hash version = %d, want %d", app3.Version, app.Version+1)
	}
	if len(g.Importers("src/Child.tsx")) != 0 {
		t.Fatal("移除依赖边后 importers 应清空")
	}

	g.Set(Node{ID: "src/Child.tsx", URL: "/src/Child.tsx", Kind: KindJS})
	g.Remove("src/Child.tsx")
	if _, ok := g.Node("src/Child.tsx"); ok {
		t.Fatal("Remove 后节点仍存在")
	}
	if len(g.Nodes()) != 1 || g.Nodes()[0].ID != "src/App.tsx" {
		t.Fatalf("Nodes = %+v", g.Nodes())
	}
}

// Remove 保留 importers 记录：dev 里"删除后又在同一防抖窗口重建"以及
// "稍后恢复同一文件"都靠它继续找到边界；importer 被重新服务且不再
// import 该模块时，反向边会自然消失。
func TestGraphRemoveKeepsImportersForRecovery(t *testing.T) {
	g := NewGraph()
	g.Set(Node{ID: "src/App.tsx", URL: "/src/App.tsx", Kind: KindJS, Imports: []Import{
		{Specifier: "./gone", Resolved: "src/gone.ts", Kind: "import-statement"},
	}})
	g.Set(Node{ID: "src/gone.ts", URL: "/src/gone.ts", Kind: KindJS})

	g.Remove("src/gone.ts")
	if got := g.Importers("src/gone.ts"); len(got) != 1 || got[0] != "src/App.tsx" {
		t.Fatalf("Remove 后应保留 importers: %v", got)
	}
	g.Set(Node{ID: "src/gone.ts", URL: "/src/gone.ts", Kind: KindJS, Hash: "recreated"})
	if got := g.Importers("src/gone.ts"); len(got) != 1 {
		t.Fatalf("重建后 importers 丢失: %v", got)
	}

	g.Set(Node{ID: "src/App.tsx", URL: "/src/App.tsx", Kind: KindJS, Hash: "changed"})
	if got := g.Importers("src/gone.ts"); len(got) != 0 {
		t.Fatalf("importer 去掉依赖后反向边应清空: %v", got)
	}
}

func TestGraphNilSafe(t *testing.T) {
	var g *Graph
	if _, ok := g.Node("x"); ok {
		t.Fatal("nil graph 不应有节点")
	}
	if g.Importers("x") != nil || g.Nodes() != nil {
		t.Fatal("nil graph 查询应为空")
	}
	g.Remove("x")
	_ = g.Set(Node{ID: "x"})
}
