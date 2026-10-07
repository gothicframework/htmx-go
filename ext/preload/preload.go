//go:build js && wasm

// Package preload is a faithful Go/WASM port of the htmx "preload" extension
// (htmx-ext-preload v2.0.1, https://github.com/bigskysoftware/htmx-extensions).
//
// It preloads the targets of elements that carry a `preload` attribute (and, for
// containers, their descendant anchors / hx-get elements), so the response is
// warm by the time the user commits to navigating. As with the JS extension it is
// activated per-subtree via hx-ext="preload".
//
// It is a sibling to the htmx-go core port: it depends only on the browser and
// the public window.htmx API (defineExtension / ajax / trigger), so it links
// independently. Call Register() once after htmx is installed (htmx.Boot):
//
//	htmx.Boot()
//	preload.Register()
//
// Versioning tracks the upstream extension (v2.0.1); the reference JS is kept
// beside this file as preload.js for line-by-line diffing.
package preload

import (
	"strings"
	"syscall/js"
)

var global = js.Global()

func htmx() js.Value     { return global.Get("htmx") }
func document() js.Value { return global.Get("document") }

// jstr returns a js.Value's string, or "" for null/undefined/non-string.
func jstr(v js.Value) string {
	if v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// Register defines the "preload" extension on window.htmx. Call once, after htmx
// has installed window.htmx. The extension only acts on elements within an
// hx-ext="preload" subtree, exactly like the JS version.
func Register() {
	ext := global.Get("Object").New()
	ext.Set("onEvent", js.FuncOf(onEvent))
	htmx().Call("defineExtension", "preload", ext)
}

func onEvent(_ js.Value, args []js.Value) any {
	// Only take action on htmx:after:init.
	if args[0].String() != "htmx:after:init" {
		return nil
	}
	event := args[1]
	parent := event.Get("target")
	if !parent.Truthy() {
		parent = event.Get("detail").Get("elt")
	}
	// Search for all child nodes that have a "preload" attribute.
	parent.Call("querySelectorAll", "[preload]").Call("forEach", js.FuncOf(func(_ js.Value, a []js.Value) any {
		node := a[0]
		initNode(node)
		// Initialize child anchors / hx-get elements (use with care).
		node.Call("querySelectorAll", "a,[hx-get],[data-hx-get]").Call("forEach", js.FuncOf(func(_ js.Value, b []js.Value) any {
			initNode(b[0])
			return nil
		}))
		return nil
	}))
	return nil
}

// attr gets the closest non-empty value from the attribute (hx- or data-hx-).
func attr(node js.Value, property string) js.Value {
	if !node.Truthy() {
		return js.Undefined()
	}
	if v := node.Call("getAttribute", property); v.Truthy() {
		return v
	}
	if v := node.Call("getAttribute", "data-"+property); v.Truthy() {
		return v
	}
	return attr(node.Get("parentElement"), property)
}

// doLoad performs the preload fetch. It mirrors the guarded closure that htmx's
// load(node) returns: it no-ops unless the node's preloadState is "READY".
func doLoad(node js.Value) {
	if jstr(node.Get("preloadState")) != "READY" {
		return
	}
	// done marks the content loaded (and optionally warms linked images).
	done := func(html js.Value) {
		if !node.Get("preloadAlways").Truthy() {
			node.Set("preloadState", "DONE")
		}
		if jstr(attr(node, "preload-images")) == "true" {
			document().Call("createElement", "div").Set("innerHTML", html)
		}
	}

	// Special handling for hx-get: use htmx.ajax so headers match a real htmx request.
	hxGet := node.Call("getAttribute", "hx-get")
	if !hxGet.Truthy() {
		hxGet = node.Call("getAttribute", "data-hx-get")
	}
	if hxGet.Truthy() {
		opts := global.Get("Object").New()
		opts.Set("source", node)
		opts.Set("handler", js.FuncOf(func(_ js.Value, a []js.Value) any {
			done(a[1].Get("xhr").Get("responseText"))
			return nil
		}))
		htmx().Call("ajax", "GET", hxGet, opts)
		return
	}

	// Otherwise a plain XHR GET against href.
	if href := node.Call("getAttribute", "href"); href.Truthy() {
		r := global.Get("XMLHttpRequest").New()
		r.Call("open", "GET", href)
		r.Set("onload", js.FuncOf(func(js.Value, []js.Value) any {
			done(r.Get("responseText"))
			return nil
		}))
		r.Call("send")
	}
}

// initNode sets up preload event handlers for a single node, exactly once.
func initNode(node js.Value) {
	// If this node has no GET target (href / hx-get / data-hx-get), nothing to do.
	href := node.Call("getAttribute", "href")
	hxg := node.Call("getAttribute", "hx-get")
	dhxg := node.Call("getAttribute", "data-hx-get")
	if href.Type() == js.TypeNull && hxg.Type() == js.TypeNull && dhxg.Type() == js.TypeNull {
		return
	}
	// Guarantee we only initialize each node once.
	if node.Get("preloadState").Type() != js.TypeUndefined {
		return
	}

	// Event name from config (default mousedown), with optional "always".
	on := "mousedown"
	if a := attr(node, "preload"); a.Truthy() {
		on = a.String()
	}
	always := strings.Contains(on, "always")
	if always {
		on = strings.TrimSpace(strings.Replace(on, "always", "", 1))
	}

	// Apply the primary listener.
	node.Call("addEventListener", on, js.FuncOf(func(_ js.Value, _ []js.Value) any {
		if jstr(node.Get("preloadState")) == "PAUSE" { // only fire once per cycle
			node.Set("preloadState", "READY") // required for doLoad to trigger
			if on == "mouseover" {
				// Wait 100ms before triggering on hover.
				global.Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
					doLoad(node)
					return nil
				}), 100)
			} else {
				doLoad(node)
			}
		}
		return nil
	}))

	// Special handling for certain built-in events.
	switch on {
	case "mouseover":
		node.Call("addEventListener", "touchstart", js.FuncOf(func(js.Value, []js.Value) any {
			doLoad(node)
			return nil
		}))
		node.Call("addEventListener", "mouseout", js.FuncOf(func(_ js.Value, a []js.Value) any {
			evt := a[0]
			if evt.Get("target").Equal(node) && jstr(node.Get("preloadState")) == "READY" {
				node.Set("preloadState", "PAUSE")
			}
			return nil
		}))
	case "mousedown":
		node.Call("addEventListener", "touchstart", js.FuncOf(func(js.Value, []js.Value) any {
			doLoad(node)
			return nil
		}))
	}

	// Mark the node ready to run.
	node.Set("preloadState", "PAUSE")
	node.Set("preloadAlways", always)
	htmx().Call("trigger", node, "preload:init") // can be used to load immediately
}
