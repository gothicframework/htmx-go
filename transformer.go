//go:build js && wasm

package htmx

import "syscall/js"

// RequestTransformer mutates an in-flight htmx request at the point where htmx
// finalizes the request — after the request configuration has been settled and
// immediately before the XHR is sent. It is the Go-native counterpart of a JS
// request-configuration listener: a transformer sees the FINAL verb, path,
// headers, and parameters, and any mutation it makes reaches the wire.
//
// Transformers are the seam that downstream request signers hook into. An AWS
// SigV4 signer, for example, needs to read the final parameters/body and set an
// authorization header (x-amz-content-sha256, Authorization, …). Unlike a JS
// request-configuration listener, a transformer is pure Go — no round-trip
// through the JS `detail` object — so an extension such as ext/sigv4 can
// reimplement the AWS signer against this type rather than against the JS event
// detail.
//
// Transformers run in registration order (see RegisterTransformer); a later
// transformer sees the mutations of the earlier ones.
type RequestTransformer func(req *TransformableRequest)

// TransformableRequest is the mutable, in-path request handed to each
// RequestTransformer. Verb, Path, and UseURLParams are plain settable fields;
// headers and parameters are mutated through the accessor methods, which write
// straight into the underlying JS objects that back the outgoing request.
type TransformableRequest struct {
	// Verb is the (lowercase) HTTP method — "get", "post", … Settable.
	Verb string
	// Path is the request path, possibly carrying a query string / anchor.
	// Settable.
	Path string
	// UseURLParams reports whether the parameters are encoded into the URL
	// query (true for GET/DELETE by default) instead of the request body.
	// Settable; a transformer that moves params between URL and body updates it.
	UseURLParams bool

	headers js.Value // plain JS object: header name -> string value
	params  js.Value // FormData holding the request parameters
	elt     js.Value // the triggering element (read-only request context)
}

// SetHeader sets (overwriting) a request header. This is how a signer adds an
// Authorization / x-amz-* header.
func (r *TransformableRequest) SetHeader(name, value string) {
	r.headers.Set(name, value)
}

// GetHeader returns a request header value, or "" if it is unset/non-string.
func (r *TransformableRequest) GetHeader(name string) string {
	return jstr(r.headers.Get(name))
}

// Headers returns the raw headers object (a plain JS object) for advanced use.
func (r *TransformableRequest) Headers() js.Value { return r.headers }

// SetParam sets (replacing any existing values for name) a request parameter.
// This is how a signer adds a signature parameter that must reach the body/URL.
func (r *TransformableRequest) SetParam(name, value string) {
	r.params.Call("set", name, value)
}

// AppendParam appends a request parameter without replacing existing values.
func (r *TransformableRequest) AppendParam(name, value string) {
	r.params.Call("append", name, value)
}

// GetParam returns the first value of a request parameter, or "" if unset.
func (r *TransformableRequest) GetParam(name string) string {
	return jstr(r.params.Call("get", name))
}

// Params returns the raw FormData holding the request parameters.
func (r *TransformableRequest) Params() js.Value { return r.params }

// EncodedBody returns the parameters as an application/x-www-form-urlencoded
// string — the exact body a signer signs when the params travel in the body.
func (r *TransformableRequest) EncodedBody() string { return urlEncode(r.params) }

// Element returns the element that triggered the request (read-only context).
func (r *TransformableRequest) Element() js.Value { return r.elt }

// requestTransformers is the ordered registry of transformers. It is only
// mutated from RegisterTransformer, which runs on the single wasm goroutine
// during boot/setup, so no synchronisation is required.
var requestTransformers []RequestTransformer

// RegisterTransformer appends a RequestTransformer to the registry. Transformers
// run in registration order inside issueAjaxRequest, after htmx:config:request
// resolves and before the XHR is sent. Register between htmx.Install() and
// htmx.Start() (or any time before requests are issued).
func RegisterTransformer(t RequestTransformer) {
	if t == nil {
		return
	}
	requestTransformers = append(requestTransformers, t)
}

// applyRequestTransformers runs every registered transformer in order against
// the final request. headers and params are mutated in place (their JS objects
// back the outgoing request); the possibly-updated verb/path/useUrlParams are
// returned to the caller.
func applyRequestTransformers(verb, path string, headers, params js.Value, useUrlParams bool, elt js.Value) (string, string, bool) {
	if len(requestTransformers) == 0 {
		return verb, path, useUrlParams
	}
	req := &TransformableRequest{
		Verb:         verb,
		Path:         path,
		UseURLParams: useUrlParams,
		headers:      headers,
		params:       params,
		elt:          elt,
	}
	for _, t := range requestTransformers {
		t(req)
	}
	return req.Verb, req.Path, req.UseURLParams
}
