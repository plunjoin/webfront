// Package vue registers Vue single-file components with the dev adapter
// registry. It intentionally contains no compiler dependency: the dev server
// still calls compiler.CompileSFCResult for the actual transform, but the
// adapter is the single source of truth for "what extensions are watched" and
// "which modules are HMR boundaries".
package vue

import (
	"path"
	"strings"

	"github.com/plunjoin/webfront/adapter"
)

// Adapter claims .vue files. Every SFC is a self-accepting HMR boundary: Vue's
// runtime patches the existing component definition in place, so nested
// component state survives an update.
type Adapter struct{}

var _ adapter.Adapter = Adapter{}

func (Adapter) Name() string { return "vue" }

func (Adapter) Extensions() []string { return []string{".vue"} }

func (Adapter) SelfAccepts(id string) bool {
	if i := strings.IndexAny(id, "?#"); i >= 0 {
		id = id[:i]
	}
	return strings.EqualFold(path.Ext(id), ".vue")
}

func (Adapter) HMRKind(string) string { return "vue" }
