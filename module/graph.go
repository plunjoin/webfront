package module

import (
	"sort"
	"sync"
)

// Kind is the coarse module type exposed to HMR planning and adapters.
type Kind string

const (
	KindJS    Kind = "js"
	KindCSS   Kind = "css"
	KindVue   Kind = "vue"
	KindJSON  Kind = "json"
	KindAsset Kind = "asset"
	KindOther Kind = "other"
)

// Import is one static edge found while transforming a module.
//
// Specifier is the source text (query included). Resolved is the canonical
// module ID for local files; it is empty for npm packages and unresolved
// specifiers. External is true when the browser/runtime resolves the
// specifier itself (import map, URL, data: URI, …).
type Import struct {
	Specifier string `json:"specifier"`
	Resolved  string `json:"resolved,omitempty"`
	Kind      string `json:"kind,omitempty"`
	External  bool   `json:"external,omitempty"`
	Asset     bool   `json:"asset,omitempty"`
}

// Node is an immutable snapshot of one module in the graph.
type Node struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Version int64  `json:"version"`
	Kind    Kind   `json:"kind"`
	Hash    string `json:"hash,omitempty"`
	// SelfAccept marks an HMR boundary: a change to this module can be applied
	// without bubbling to an importer. HMRKind is the wire kind sent to the
	// browser ("vue", "css", …); empty falls back to Kind.
	SelfAccept bool     `json:"selfAccept,omitempty"`
	HMRKind    string   `json:"hmrKind,omitempty"`
	Imports    []Import `json:"imports,omitempty"`
	Importers  []string `json:"importers,omitempty"`
}

// Graph is a concurrency-safe module graph. Nodes are keyed by Key(ID); the
// reverse index powers imports/importers queries without scanning every node.
type Graph struct {
	mu        sync.RWMutex
	nodes     map[string]*Node
	importers map[string]map[string]struct{} // target key -> importer IDs
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		nodes:     map[string]*Node{},
		importers: map[string]map[string]struct{}{},
	}
}

// Set inserts or updates a node and returns its snapshot. Version is managed
// by the graph: an unchanged hash and URL keep the previous version, so
// repeated dev requests do not churn HMR clients.
func (g *Graph) Set(n Node) Node {
	if g == nil {
		return n
	}
	key := Key(n.ID)
	g.mu.Lock()
	old := g.nodes[key]
	if old != nil {
		g.removeEdgesLocked(old)
		if n.Hash != "" && n.Hash == old.Hash && n.URL == old.URL {
			n.Version = old.Version
		} else {
			n.Version = old.Version + 1
		}
	}
	if n.Version == 0 {
		n.Version = 1
	}
	n.Imports = append([]Import(nil), n.Imports...)
	stored := n
	g.nodes[key] = &stored
	g.addEdgesLocked(&stored)
	g.mu.Unlock()
	snap, _ := g.Node(n.ID)
	return snap
}

// Remove drops a node and its outgoing edges. Incoming importer records are
// kept on purpose: dev HMR uses them to recover when a file is re-created
// (delete + create in one editor save, or a module brought back later), and
// they shrink naturally when an importer is re-served without that edge.
// Nodes() never lists a removed module; only Importers(id) can still answer.
func (g *Graph) Remove(id string) {
	if g == nil {
		return
	}
	key := Key(id)
	g.mu.Lock()
	if old := g.nodes[key]; old != nil {
		g.removeEdgesLocked(old)
		delete(g.nodes, key)
	}
	g.mu.Unlock()
}

// Node returns a snapshot of one module.
func (g *Graph) Node(id string) (Node, bool) {
	if g == nil {
		return Node{}, false
	}
	key := Key(id)
	g.mu.RLock()
	n := g.nodes[key]
	if n == nil {
		g.mu.RUnlock()
		return Node{}, false
	}
	snap := *n
	snap.Imports = append([]Import(nil), n.Imports...)
	snap.Importers = g.importersLocked(key)
	g.mu.RUnlock()
	return snap, true
}

// Nodes returns every module sorted by ID.
func (g *Graph) Nodes() []Node {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	out := make([]Node, 0, len(g.nodes))
	for key, n := range g.nodes {
		snap := *n
		snap.Imports = append([]Import(nil), n.Imports...)
		snap.Importers = g.importersLocked(key)
		out = append(out, snap)
	}
	g.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Importers returns the IDs of modules that statically import id.
func (g *Graph) Importers(id string) []string {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	out := g.importersLocked(Key(id))
	g.mu.RUnlock()
	return out
}

func (g *Graph) importersLocked(key string) []string {
	set := g.importers[key]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for importer := range set {
		out = append(out, importer)
	}
	sort.Strings(out)
	return out
}

func (g *Graph) addEdgesLocked(n *Node) {
	for _, imp := range n.Imports {
		if imp.External || imp.Resolved == "" {
			continue
		}
		target := Key(imp.Resolved)
		if g.importers[target] == nil {
			g.importers[target] = map[string]struct{}{}
		}
		g.importers[target][n.ID] = struct{}{}
	}
}

func (g *Graph) removeEdgesLocked(n *Node) {
	for _, imp := range n.Imports {
		if imp.External || imp.Resolved == "" {
			continue
		}
		target := Key(imp.Resolved)
		if set := g.importers[target]; set != nil {
			delete(set, n.ID)
			if len(set) == 0 {
				delete(g.importers, target)
			}
		}
	}
}
