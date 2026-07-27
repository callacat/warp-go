// Package frontui embeds the metacubexd control-panel frontend into the warp-go
// binary and exposes the runtime extraction seam that turns the embedded fs into a
// on-disk directory mihomo's external-controller-ui can serve from.
//
// frontui is a pure-stdlib package (embed + io/fs + os): it has zero mihomo
// imports. The anti-corruption direction is one-way — frontproxy/mihomo imports
// frontui to consume DistFS + Extract, never the reverse — mirroring the
// frontrender "independent seam" decision (see frontproxy/frontrender/render.go
// doc comment) so the embed/extract layer can be unit-tested and evolved without
// a compiled mihomo kernel.
//
// Why //go:embed carries the `all:` prefix: metacubexd (a Nuxt static export,
// v1.270.5) lays its JS/CSS bundle under `assets/metacubexd/_nuxt/` and its fonts
// under `assets/metacubexd/_fonts/`. Go's //go:embed skips any path whose first
// segment begins with `_` or `.`, so a bare `//go:embed assets/metacubexd` would
// drop `_nuxt/` (109 JS/CSS files) and `_fonts/` (32 woff2) entirely — the panel
// would load index.html and 404 every script/stylesheet. `all:` is the only
// toggle that forces those underscored trees into the binary. This is the single
// most fragile part of P3 and the reason embed_test.go asserts file count ≥ 100
// rather than trusting "index.html present" alone.
package frontui

import "embed"

// DistFS is the embedded metacubexd distribution root. Its logical root is the
// content of the assets/metacubexd directory (index.html, _nuxt/, _fonts/,
// config.js, PWA icons, …), NOT the metacubexd subdirectory itself. Callers
// that want the panel files must WalkDir against the root "" and strip no prefix;
// the mihomo-side wiring extracts the subtree rooted at "assets/metacubexd".
//
// In a clean checkout (no vendoring run) DistFS contains only the placeholder
// index.html — enough to compile and to boot an empty panel, NOT enough to render
// the real metacubexd UI. The placeholder is the floor; vendoring is the ceiling.
// embed_test.go's file-count assertion makes the gap a visible CI signal.
//
//go:embed all:assets/metacubexd
var DistFS embed.FS
