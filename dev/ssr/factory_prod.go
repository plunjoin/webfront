//go:build !dev

package ssr

import "github.com/plunjoin/webfront/dev"

func factory(workers int) dev.SSRRendererFactory {
	_ = workers
	return nil
}
