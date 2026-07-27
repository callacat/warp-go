// Package mihomo is the anti-corruption layer that is the ONLY package
// allowed to import github.com/metacubex/mihomo/... in this module.
// PoC file: proves mihomo-as-library integration compiles alongside
// warp-go's own quic-go v0.61.0 (mihomo uses metacubex/quic-go fork).
package mihomo

import (
	"github.com/metacubex/mihomo/hub"
	"github.com/metacubex/mihomo/hub/executor"
	"github.com/metacubex/mihomo/hub/route"
)

// smokePoC proves the library-mode entry points resolve at compile time.
// It is intentionally never called at runtime in the PoC.
func smokePoC() {
	_ = route.SetEmbedMode
	_ = hub.WithExternalController
	_ = hub.WithExternalUI
	_ = hub.WithSecret
	_ = hub.Parse
	_ = executor.Shutdown
}
