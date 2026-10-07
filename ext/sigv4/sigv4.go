//go:build js && wasm

package sigv4

import (
	"strings"
	"syscall/js"

	htmx "github.com/gothicframework/htmx-go/v4"
)

// providerMetaSelector locates the server-rendered provider marker in the parsed
// SSR <head>: <meta name="gothic-provider" content="AWS">. RuntimeScripts emits
// it (only when GOTHIC_PROVIDER=="AWS") long before core.wasm boots, so reading
// it at Register() time is race-free — the head is fully parsed by then. There is
// no request-holding shim and no boot handshake: the gate is one static meta tag.
const providerMetaSelector = `meta[name="gothic-provider"]`

// awsProvider is the marker content that turns signing on. Any other value (or a
// missing marker, e.g. dev / non-AWS) leaves the signer dormant: Register()
// installs NO transformer, so no x-amz-content-sha256 header is ever emitted.
const awsProvider = "AWS"

// Register installs the AWS SigV4 body-hash transformer on htmx's in-path request
// pipeline, but ONLY when the server rendered the AWS provider marker on this
// page. Call it once, between htmx.Install() and htmx.Start(), so the transformer
// is registered before any request is issued.
//
// The gate is a RUNTIME signal, not compile-time: core.wasm is a single committed
// binary served identically to every Gothic app, so signing cannot be decided at
// wasm-build time. On AWS the server-rendered <head> carries
// <meta name="gothic-provider" content="AWS">; off AWS it does not. Register reads
// that marker and, when it is absent or not "AWS", returns without registering
// anything — making an unsigned-vs-signed mistake impossible by control flow
// rather than by a timing shim.
//
// When enabled, the transformer runs in-path (after htmx:config:request, before
// xhr.send), reproduces htmx's exact body, and sets
// x-amz-content-sha256 = sha256hex(body). Because it is a line in the request
// path — not a racing JS listener — emitting an unsigned request is impossible by
// construction.
func Register() {
	if !providerIsAWS() {
		return // dormant off AWS: no transformer, header absent.
	}
	htmx.RegisterTransformer(signRequest)
}

// providerIsAWS reports whether the server-rendered provider marker says AWS. It
// reads document.querySelector('meta[name="gothic-provider"]').content once at
// registration time; the marker is static SSR HTML present before the WASM boots,
// so no race and no shim is involved.
func providerIsAWS() bool {
	doc := js.Global().Get("document")
	if !doc.Truthy() {
		return false
	}
	meta := doc.Call("querySelector", providerMetaSelector)
	if !meta.Truthy() {
		return false
	}
	content := meta.Call("getAttribute", "content")
	if content.Type() != js.TypeString {
		return false
	}
	return content.String() == awsProvider
}

// signRequest is the in-path RequestTransformer, mapping htmx's request onto the
// AWS body-hash decision tree against the Go-native TransformableRequest:
//
//   - UseURLParams (GET/DELETE etc.): params go in the URL and the body is empty
//     → EmptyBodyHash.
//   - multipart/form-data: the browser builds a random-boundary body we cannot
//     reproduce, so a computed hash would never match → UNSIGNED-PAYLOAD sentinel.
//     This is checked BEFORE the empty-body branch on purpose: an EMPTY multipart
//     form (hx-encoding=multipart/form-data with no successful controls) still
//     puts the closing boundary bytes on the wire, so its body is NOT empty and
//     signing sha256("") would be wrong (CloudFront 403). Multipart always
//     escapes to the sentinel regardless of parameter count.
//   - empty body (non-multipart, no parameters): → EmptyBodyHash. A truly-empty
//     urlencoded POST carries Content-Type application/x-www-form-urlencoded, so
//     isMultipartRequest is false and it correctly falls through here.
//   - otherwise: sha256hex of htmx's OWN urlencoded body string
//     (req.EncodedBody() == htmx's urlEncode(params)), guaranteeing the signed
//     hash matches the bytes htmx sends byte-for-byte.
//
// It always sets the x-amz-content-sha256 header.
func signRequest(req *htmx.TransformableRequest) {
	var hash string
	switch {
	case req.UseURLParams:
		hash = EmptyBodyHash
	case isMultipartRequest(req):
		hash = UnsignedPayload
	default:
		// EncodedBody is htmx's urlEncode over the SAME FormData htmx will send,
		// so it is byte-identical to the request body by construction. Empty here
		// means a genuinely empty urlencoded body → sha256("") == EmptyBodyHash.
		if body := req.EncodedBody(); body != "" {
			hash = Sha256Hex(body)
		} else {
			hash = EmptyBodyHash
		}
	}
	req.SetHeader(ContentSha256Header, hash)
}

// isMultipartRequest reports whether htmx will send this request as
// multipart/form-data (a random-boundary body we cannot reproduce). htmx sets
// Content-Type = "application/x-www-form-urlencoded" for a normal non-GET body
// and leaves it UNSET for a multipart body (the browser fills in the boundary).
// So: Content-Type absent-or-not-urlencoded ⇒ treat as multipart. GetHeader
// returns "" for an unset/non-string header, which correctly reads as multipart.
func isMultipartRequest(req *htmx.TransformableRequest) bool {
	ct := req.GetHeader("Content-Type")
	if ct == "" {
		return true
	}
	return !strings.HasPrefix(strings.ToLower(ct), "application/x-www-form-urlencoded")
}
