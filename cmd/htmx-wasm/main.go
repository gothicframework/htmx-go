//go:build js && wasm

// Command htmx-wasm builds the Go/WASM htmx mirror into a standalone core.wasm.
//
// This is how a consumer (e.g. the Gothic core runtime) boots the library: import
// the package and call htmx.Boot() from a js/wasm main, then block forever so the
// installed event handlers stay alive.
package main

import htmx "github.com/gothicframework/htmx-go/v4"

func main() {
	htmx.Boot()
	select {} // keep the Go runtime (and htmx's listeners) alive
}
