//go:build js && wasm

// Command htmx-preload-wasm builds a standalone wasm for the browser validation
// harness. It exercises three things in one binary: the core htmx port, the
// preload extension, and the Go-native RequestTransformer seam.
//
// Boot ordering matters. It installs window.htmx with htmx.Install() (which
// exposes the extension + transformer APIs but does NOT process the document),
// then registers the preload extension and the request transformers, and only
// then runs htmx.Start() to process the DOM. Registering the extension before
// the initial processNode(body) is what lets static hx-ext="preload" markup
// initialise; registering the transformers before any request lets them mutate
// every outgoing request.
package main

import (
	htmx "github.com/gothicframework/htmx-go/v2"
	"github.com/gothicframework/htmx-go/v2/ext/preload"
)

func main() {
	htmx.Install() // install window.htmx + extension/transformer APIs (no DOM pass)

	preload.Register() // define the "preload" extension before the initial DOM pass

	// Register request transformers to prove the seam end-to-end in the harness.
	// Two of them append to the same header so registration ORDER is observable
	// on the wire ("1" then "2" => "12"); a third adds a signature PARAMETER,
	// proving the sigv4-style param contract (a transformer's param reaches the
	// body). /echo reflects both back for the Playwright spec to assert.
	htmx.RegisterTransformer(func(r *htmx.TransformableRequest) {
		r.SetHeader("X-Transform-Order", r.GetHeader("X-Transform-Order")+"1")
	})
	htmx.RegisterTransformer(func(r *htmx.TransformableRequest) {
		r.SetHeader("X-Transform-Order", r.GetHeader("X-Transform-Order")+"2")
	})
	htmx.RegisterTransformer(func(r *htmx.TransformableRequest) {
		r.SetParam("xform-sig", "sig-from-transformer")
	})

	htmx.Start() // now process the document (extensions + transformers already registered)

	select {} // keep the Go runtime (and htmx's listeners) alive
}
