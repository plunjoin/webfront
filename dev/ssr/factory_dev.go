//go:build dev

package ssr

import (
	"github.com/plunjoin/webfront/dev"
	"github.com/plunjoin/webfront/jsruntime"
)

func factory(workers int) dev.SSRRendererFactory {
	return func(bundle []byte) (dev.SSRRenderer, error) {
		return jsruntime.New(bundle, workers)
	}
}
