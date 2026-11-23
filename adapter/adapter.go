// Package adapter defines the small, framework-facing seam used by the dev
// server to discover non-JavaScript module types and their HMR boundaries.
//
// The package is deliberately stdlib-only: production builds compile it through
// dev.Option, so it must not pull esbuild, fsnotify or websocket into a normal
// binary. Adapter implementations live in their own packages (adapter/vue) and
// are injected by the CLI or the application; compiler and dev never import a
// concrete adapter from their core files.
package adapter

import (
	"path"
	"sort"
	"strings"
)

// Adapter describes one source format. Empty extensions mean the adapter does
// not claim files by suffix (it may still be consulted via other APIs later).
//
// SelfAccepts reports whether a change to id can be applied in place without
// bubbling to an importer. HMRKind is the wire-level kind sent to the browser
// ("vue", "css", …); an empty value lets the dev server fall back to the
// module kind recorded in the graph.
type Adapter interface {
	Name() string
	Extensions() []string
	SelfAccepts(id string) bool
	HMRKind(id string) string
}

// Registry maps extensions to adapters. It is built once at server startup and
// then read-only, so it needs no locking.
type Registry struct {
	adapters []Adapter
	byExt    map[string]Adapter
}

// New returns a registry containing adapters in the given order. Later
// adapters win when two claim the same extension.
func New(adapters ...Adapter) *Registry {
	r := &Registry{byExt: map[string]Adapter{}}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// Register adds an adapter. nil and duplicate nil-safe values are ignored.
func (r *Registry) Register(a Adapter) {
	if r == nil || a == nil {
		return
	}
	if r.byExt == nil {
		r.byExt = map[string]Adapter{}
	}
	r.adapters = append(r.adapters, a)
	for _, ext := range a.Extensions() {
		ext = normalizeExt(ext)
		if ext != "" {
			r.byExt[ext] = a
		}
	}
}

// Adapters returns a copy of the adapter list in registration order.
func (r *Registry) Adapters() []Adapter {
	if r == nil || len(r.adapters) == 0 {
		return nil
	}
	return append([]Adapter(nil), r.adapters...)
}

// Extensions returns the claimed extensions, normalized to lower-case with a
// leading dot, sorted and de-duplicated. It is the dev server's watch filter.
func (r *Registry) Extensions() []string {
	if r == nil || len(r.byExt) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.byExt))
	for ext := range r.byExt {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// For returns the adapter claiming id's extension, or nil.
func (r *Registry) For(id string) Adapter {
	if r == nil {
		return nil
	}
	return r.byExt[normalizeExt(path.Ext(trimQuery(id)))]
}

// SelfAccepts reports whether the adapter for id declares it an HMR boundary.
func (r *Registry) SelfAccepts(id string) bool {
	a := r.For(id)
	return a != nil && a.SelfAccepts(trimQuery(id))
}

// HMRKind returns the adapter's wire kind for id, or "" when unclaimed.
func (r *Registry) HMRKind(id string) string {
	a := r.For(id)
	if a == nil {
		return ""
	}
	return a.HMRKind(trimQuery(id))
}

// Names returns the registered adapter names, sorted and de-duplicated.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(r.adapters))
	for _, a := range r.adapters {
		name := strings.TrimSpace(a.Name())
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether an adapter with the given name is registered.
func (r *Registry) Has(name string) bool {
	for _, n := range r.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

func trimQuery(id string) string {
	if i := strings.IndexAny(id, "?#"); i >= 0 {
		return id[:i]
	}
	return id
}
