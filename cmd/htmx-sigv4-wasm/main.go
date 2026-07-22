//go:build js && wasm

// Command htmx-sigv4-wasm builds a standalone wasm for the browser validation
// harness that mirrors the Gothic core boot order for AWS request signing: it
// installs window.htmx, registers the sigv4 body-hash signer and the preload
// extension, then processes the DOM.
//
// The sigv4 signer is RUNTIME-gated on the server-rendered
// <meta name="gothic-provider" content="AWS"> marker: on a page that carries it,
// sigv4.Register installs the in-path transformer and every htmx request gets an
// x-amz-content-sha256 header; on a page WITHOUT the marker, Register installs
// nothing and no header is emitted. The harness serves both page variants so the
// spec can assert the gate in both directions.
//
// Boot ordering matches core/wasm/core-runtime: Install() (window.htmx, no DOM
// pass) → sigv4.Register() → preload.Register() → Start() (process the DOM). The
// signer must be registered before Start so it is in place before the first
// request (a boot-time hx-trigger="load"/every) is issued.
package main

import (
	htmx "github.com/gothicframework/htmx-go/v2"
	"github.com/gothicframework/htmx-go/v2/ext/preload"
	"github.com/gothicframework/htmx-go/v2/ext/sigv4"
)

func main() {
	htmx.Install()     // install window.htmx + extension/transformer APIs (no DOM pass)
	sigv4.Register()   // register the AWS body-hash signer IF the provider marker says AWS
	preload.Register() // define the "preload" extension before the initial DOM pass

	htmx.Start() // now process the document (signer + extension already registered)

	select {} // keep the Go runtime (and htmx's listeners) alive
}
