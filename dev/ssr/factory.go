// Package ssr wires the optional development-time SSR renderer.
//
// Importing this package in a normal (non-dev) build is free: Factory returns
// nil and jsruntime/goja are not linked. Only -tags dev builds pull the JS
// engine, and only when the application actually calls Factory.
package ssr

import "github.com/plunjoin/webfront/dev"

// Factory creates a jsruntime-backed renderer for dev SSR. workers <= 0 uses
// the jsruntime default (2 VMs).
//
// In a production build this returns nil; dev.WithSSR then rejects the nil
// factory if it is ever reached (it is not, because dev.Handler bypasses the
// frontend entirely without -tags dev).
func Factory(workers int) dev.SSRRendererFactory {
	return factory(workers)
}
