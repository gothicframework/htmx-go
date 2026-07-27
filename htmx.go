//go:build js && wasm

// Package htmx is a faithful Go/WASM port of the htmx JavaScript library
// (https://htmx.org), maintained by the Gothic Framework org and imported by the
// core module's server-side WASM runtime in place of the vendored htmx.min.js.
//
// It is a direct, function-by-function translation of htmx v2.0.9: the same
// internal-data model, trigger tokenizer, XMLHttpRequest request cycle, swap +
// settle pipeline, history support, and extension API. Function names mirror the
// originals so the two files can be diffed side by side. Where htmx uses
// new Function/eval (event filters, hx-vals/hx-vars js:, hx-on:), this port
// delegates to the browser's JS engine via syscall/js — the faithful behaviour,
// since it runs in the browser.
//
// Versioning tracks upstream htmx (see go.mod): module .../htmx-go/v2 mirrors
// htmx 2.x; the exposed window.htmx.version reports the upstream string.
//
// Call Boot once from a js/wasm entrypoint to install window.htmx and wire the
// document.
package htmx

import (
	"regexp"
	"strconv"
	"strings"
	"syscall/js"
)

// Version is the upstream htmx release this port mirrors.
const Version = "2.0.9"

// ---------------------------------------------------------------------------
// JS handles / tiny helpers (Go equivalents of implicit JS globals)
// ---------------------------------------------------------------------------

var (
	global    = js.Global()
	null      = js.Null()
	undefined = js.Undefined()
	htmxObj   js.Value // the exposed window.htmx object
	config    js.Value // htmx.config (a live JS object)
)

func getDocument() js.Value { return global.Get("document") }
func getWindow() js.Value   { return global }
func location() js.Value    { return global.Get("location") }

func isElement(v js.Value) bool {
	// htmx.js: `elt instanceof Element`. `instanceof` is forgiving — a string or
	// number yields false, never a throw. The prior `v.Get("nodeType")` form
	// PANICS in Go when v is a truthy primitive (js.Value.Get on string/number),
	// which can kill the runtime if a non-node ever reaches asElement/matches, so
	// probe with InstanceOf (JS `instanceof`, safe on any value type) instead.
	return v.InstanceOf(global.Get("Element"))
}

// asElement returns v if it is an Element, else null (mirrors htmx asElement).
func asElement(v js.Value) js.Value {
	if isElement(v) {
		return v
	}
	return null
}

func isFunction(v js.Value) bool { return v.Type() == js.TypeFunction }

// isObjectOrFunction reports whether v is safe to call js.Value.Get on. Get
// panics on strings/numbers/booleans/null/undefined, so guard property probes
// (e.g. `value.forEach`) with this to mirror JS's forgiving member access.
func isObjectOrFunction(v js.Value) bool {
	return v.Type() == js.TypeObject || v.Type() == js.TypeFunction
}

// truthy mirrors JS truthiness for our js.Value returns.
func truthy(v js.Value) bool { return v.Truthy() }

// jstr converts a js.Value to a Go string ("" for null/undefined/non-string).
func jstr(v js.Value) string {
	if v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// safe runs fn, returning true if a JS exception was thrown (Call panics with a
// js.Error on a thrown JS exception).
//
// It only contains anything on a toolchain that implements recover for the target:
// TinyGo has no recover on wasm32, where a panic prints and traps instead of
// reaching the defer. Prefer jsTry/jsTryNew/jsTryGet, which move the try/catch into
// JS and work on every toolchain; safe is the last resort for a throw that can only
// arise from Go code, which no JS try/catch can wrap.
func safe(fn func()) (threw bool) {
	defer func() {
		if recover() != nil {
			threw = true
		}
	}()
	fn()
	return false
}

// tryCallBridge resolves the host page's optional __htmxTryCall helper, caching it
// once found. It is an optional page asset, never a hard dependency of this module.
func tryCallBridge() js.Value {
	if isFunction(tryCallFn) {
		return tryCallFn
	}
	tryCallFn = global.Get("__htmxTryCall")
	return tryCallFn
}

var tryCallFn js.Value

// jsTry invokes target[method](args...) and reports whether it completed, so a throw
// from JS (a third-party extension, a user event-filter expression, a builtin
// rejecting its input) becomes a value instead of an instance-killing trap.
//
// With the host's __htmxTryCall bridge the try/catch runs in JS and holds on every
// toolchain; without it the call is direct and only a recover-capable toolchain
// contains the throw. The returned value is undefined when the call did not complete.
func jsTry(target js.Value, method string, args ...any) (js.Value, bool) {
	bridge := tryCallBridge()
	if !isFunction(bridge) {
		out := undefined
		threw := safe(func() { out = target.Call(method, args...) })
		return out, !threw
	}
	all := make([]any, 0, len(args)+2)
	all = append(all, target, method)
	all = append(all, args...)
	res := bridge.Invoke(all...)
	if res.Type() != js.TypeObject || !res.Get("ok").Truthy() {
		return undefined, false
	}
	return res.Get("v"), true
}

// jsTryNew constructs new ctor(args...) under the same protection as jsTry.
// Reflect.construct keeps it a plain method call, so one primitive covers both.
func jsTryNew(ctor js.Value, args ...any) (js.Value, bool) {
	arr := global.Get("Array").New()
	for _, a := range args {
		arr.Call("push", a)
	}
	return jsTry(global.Get("Reflect"), "construct", ctor, arr)
}

// jsTryGet reads target[prop] under the same protection as jsTry.
// A direct js.Value.Get on a property whose getter throws (window.sessionStorage in
// a sandboxed frame) is unconditionally fatal, because syscall/js only reports
// throws for Call/Invoke/New — routing the read through Reflect.get makes it one.
func jsTryGet(target js.Value, prop string) (js.Value, bool) {
	return jsTry(global.Get("Reflect"), "get", target, prop)
}

// jsSafeValue converts v for the crossing into JS, yielding undefined for a type
// js.ValueOf rejects. js.ValueOf panics on anything outside Value, Func, nil, bool,
// the numeric types, string, []any and map[string]any — a Go pointer or struct
// reaching it kills the instance.
func jsSafeValue(v any) js.Value {
	switch t := v.(type) {
	case nil:
		return null
	case js.Value:
		return t
	case js.Func:
		return t.Value
	case bool, string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64,
		[]any, map[string]any:
		return js.ValueOf(t)
	}
	global.Get("console").Call("error", "htmx: value of unsupported Go type dropped at the JS boundary")
	return undefined
}

func logError(v any) { global.Get("console").Call("error", jsSafeValue(v)) }

// newObject builds a plain JS object from key/value pairs.
func newObject() js.Value { return global.Get("Object").New() }

func jsArrayToSlice(v js.Value) []js.Value {
	out := []js.Value{}
	if !v.Truthy() {
		return out
	}
	n := v.Get("length").Int()
	for i := 0; i < n; i++ {
		out = append(out, v.Index(i))
	}
	return out
}

// ---------------------------------------------------------------------------
// Internal data model
//
// htmx stores private per-element/-event data on an expando ('htmx-internal-data').
// That data holds Go-native things (timers, closures, listener records), so we
// key a Go-side store by an id stamped onto each node/event.
// ---------------------------------------------------------------------------

type onHandler struct {
	event    string
	listener js.Func
	target   js.Value
}

type listenerInfo struct {
	trigger  string
	listener js.Func
	on       js.Value
}

// nodeData mirrors HtmxNodeInternalData.
type nodeData struct {
	initHash           int32
	hasInitHash        bool
	boosted            bool
	onHandlers         []onHandler
	timeout            js.Value
	hasTimeout         bool
	listenerInfos      []listenerInfo
	cancelled          bool
	triggeredOnce      bool
	delayed            js.Value
	hasDelayed         bool
	throttle           js.Value
	hasThrottle        bool
	lastValue          map[*triggerSpec]map[int]string // WeakMap<spec, WeakMap<elt, value>>
	loaded             bool
	path               string
	verb               string
	polling            bool
	lastButtonClicked  js.Value
	requestCount       int
	xhr                js.Value
	hasXhr             bool
	queuedRequests     []func()
	abortable          bool
	firstInitCompleted bool

	// event data
	triggerSpec *triggerSpec
	handledFor  []int
}

var (
	nextNodeID = 1
	dataStore  = map[int]*nodeData{}
)

// getInternalData retrieves the private data object for a node or event.
func getInternalData(elt js.Value) *nodeData {
	idv := elt.Get("__htmxGoId")
	var key int
	if idv.Type() != js.TypeNumber {
		key = nextNodeID
		nextNodeID++
		elt.Set("__htmxGoId", key)
	} else {
		key = idv.Int()
	}
	d := dataStore[key]
	if d == nil {
		d = &nodeData{lastButtonClicked: null}
		dataStore[key] = d
	}
	return d
}

func nodeID(elt js.Value) int {
	idv := elt.Get("__htmxGoId")
	if idv.Type() != js.TypeNumber {
		key := nextNodeID
		nextNodeID++
		elt.Set("__htmxGoId", key)
		return key
	}
	return idv.Int()
}

// ---------------------------------------------------------------------------
// Config accessors (read from the live htmx.config JS object)
// ---------------------------------------------------------------------------

func cfg(key string) js.Value  { return config.Get(key) }
func cfgBool(key string) bool  { return config.Get(key).Truthy() }
func cfgStr(key string) string { return jstr(config.Get(key)) }
func cfgFloat(key string) float64 {
	v := config.Get(key)
	if v.Type() == js.TypeNumber {
		return v.Float()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

var reInterval = struct{}{}

// parseInterval parses an htmx interval string ("500ms","2s","1m") to ms.
// Returns (0,false) for an unparseable/undefined value (htmx returns undefined).
func parseInterval(str string) (float64, bool) {
	if str == "" {
		return 0, false
	}
	var interval float64
	var err error
	switch {
	case strings.HasSuffix(str, "ms"):
		interval, err = strconv.ParseFloat(strings.TrimSpace(str[:len(str)-2]), 64)
	case strings.HasSuffix(str, "s"):
		interval, err = strconv.ParseFloat(strings.TrimSpace(str[:len(str)-1]), 64)
		interval *= 1000
	case strings.HasSuffix(str, "m"):
		interval, err = strconv.ParseFloat(strings.TrimSpace(str[:len(str)-1]), 64)
		interval *= 1000 * 60
	default:
		interval, err = strconv.ParseFloat(strings.TrimSpace(str), 64)
	}
	if err != nil {
		return 0, false
	}
	return interval, true
}

// getRawAttribute returns elt.getAttribute(name) or "" (null) for non-elements.
func getRawAttribute(elt js.Value, name string) string {
	if !isElement(elt) {
		return ""
	}
	v := elt.Call("getAttribute", name)
	if v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}

// hasAttribute resolves both hx- and data-hx- prefixes.
func hasAttribute(elt js.Value, qualifiedName string) bool {
	if !elt.Truthy() || !elt.Get("hasAttribute").Truthy() {
		return false
	}
	return elt.Call("hasAttribute", qualifiedName).Bool() ||
		elt.Call("hasAttribute", "data-"+qualifiedName).Bool()
}

// getAttributeValue resolves both hx- and data-hx- prefixes.
func getAttributeValue(elt js.Value, qualifiedName string) string {
	if v := getRawAttribute(elt, qualifiedName); v != "" {
		return v
	}
	return getRawAttribute(elt, "data-"+qualifiedName)
}

func parentElt(elt js.Value) js.Value {
	parent := elt.Get("parentElement")
	if !parent.Truthy() {
		if pn := elt.Get("parentNode"); pn.Truthy() && pn.Get("host").Truthy() {
			// ShadowRoot
			return pn
		}
	}
	return parent
}

func getRootNode(elt js.Value, global bool) js.Value {
	if elt.Get("getRootNode").Truthy() {
		opts := newObject()
		opts.Set("composed", global)
		return elt.Call("getRootNode", opts)
	}
	return getDocument()
}

// getClosestMatch walks up the parentage until condition(elt) is true.
func getClosestMatch(elt js.Value, condition func(js.Value) bool) js.Value {
	for elt.Truthy() && !condition(elt) {
		elt = parentElt(elt)
	}
	if elt.Truthy() {
		return elt
	}
	return null
}

// getAttributeValueWithDisinheritance returns "" when unset, "unset" sentinel
// when disinherited (mirrors htmx which returns the JS strings).
func getAttributeValueWithDisinheritance(initial, ancestor js.Value, attributeName string) (string, bool) {
	attributeValue := getAttributeValue(ancestor, attributeName)
	disinherit := getAttributeValue(ancestor, "hx-disinherit")
	inherit := getAttributeValue(ancestor, "hx-inherit")
	if nodeID(initial) != nodeID(ancestor) {
		if cfgBool("disableInheritance") {
			if inherit != "" && (inherit == "*" || contains(strings.Split(inherit, " "), attributeName)) {
				return attributeValue, true
			}
			return "", false
		}
		if disinherit != "" && (disinherit == "*" || contains(strings.Split(disinherit, " "), attributeName)) {
			return "unset", true
		}
	}
	if attributeValue != "" {
		return attributeValue, true
	}
	return "", false
}

// getClosestAttributeValue mirrors htmx: walks ancestors, honours disinheritance.
func getClosestAttributeValue(elt js.Value, attributeName string) (string, bool) {
	var closestAttr string
	var found bool
	getClosestMatch(elt, func(e js.Value) bool {
		v, ok := getAttributeValueWithDisinheritance(elt, asElement(e), attributeName)
		closestAttr = v
		found = ok
		return ok
	})
	if found && closestAttr != "unset" {
		return closestAttr, true
	}
	return "", false
}

func matches(elt js.Value, selector string) bool {
	return isElement(elt) && elt.Call("matches", selector).Bool()
}

var startTagRe = regexp.MustCompile(`(?i)<([a-z][^/\x00>\x20\t\r\n\f]*)`)

func getStartTag(str string) string {
	m := startTagRe.FindStringSubmatch(str)
	if m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

func parseHTML(resp string) js.Value {
	docCtor := global.Get("Document")
	if docCtor.Truthy() && docCtor.Get("parseHTMLUnsafe").Truthy() {
		return docCtor.Call("parseHTMLUnsafe", resp)
	}
	parser := global.Get("DOMParser").New()
	return parser.Call("parseFromString", resp, "text/html")
}

func takeChildrenFor(fragment, elt js.Value) {
	for elt.Get("childNodes").Get("length").Int() > 0 {
		fragment.Call("append", elt.Get("childNodes").Index(0))
	}
}

func duplicateScript(script js.Value) js.Value {
	newScript := getDocument().Call("createElement", "script")
	attrs := script.Get("attributes")
	for i := 0; i < attrs.Get("length").Int(); i++ {
		attr := attrs.Index(i)
		newScript.Call("setAttribute", attr.Get("name").String(), attr.Get("value").String())
	}
	newScript.Set("textContent", script.Get("textContent"))
	newScript.Set("async", false)
	if nonce := cfgStr("inlineScriptNonce"); nonce != "" {
		newScript.Set("nonce", nonce)
	}
	return newScript
}

func isJavaScriptScriptNode(script js.Value) bool {
	if !script.Call("matches", "script").Bool() {
		return false
	}
	t := jstr(script.Get("type"))
	return t == "text/javascript" || t == "module" || t == ""
}

func normalizeScriptTags(fragment js.Value) {
	for _, script := range jsArrayToSlice(fragment.Call("querySelectorAll", "script")) {
		if isJavaScriptScriptNode(script) {
			newScript := duplicateScript(script)
			// insertBefore cannot fail for a reference node that IS parent's child, so
			// the only failure is a detached script with no parent to insert into.
			if parent := script.Get("parentNode"); parent.Truthy() {
				parent.Call("insertBefore", newScript, script)
			} else {
				logError("htmx: failed to insert script")
			}
			script.Call("remove")
		}
	}
}

var headStripRe = regexp.MustCompile(`(?is)<head(\s[^>]*)?>.*?</head>`)

// makeFragment builds a DocumentFragment (with a .title expando) from response HTML.
func makeFragment(response string) js.Value {
	responseWithNoHead := headStripRe.ReplaceAllString(response, "")
	startTag := getStartTag(responseWithNoHead)
	var fragment js.Value
	switch startTag {
	case "html":
		fragment = global.Get("DocumentFragment").New()
		doc := parseHTML(response)
		takeChildrenFor(fragment, doc.Get("body"))
		fragment.Set("title", doc.Get("title"))
	case "body":
		fragment = global.Get("DocumentFragment").New()
		doc := parseHTML(responseWithNoHead)
		takeChildrenFor(fragment, doc.Get("body"))
		fragment.Set("title", doc.Get("title"))
	default:
		doc := parseHTML(`<body><template class="internal-htmx-wrapper">` + responseWithNoHead + `</template></body>`)
		fragment = doc.Call("querySelector", "template").Get("content")
		fragment.Set("title", doc.Get("title"))
		titleElement := fragment.Call("querySelector", "title")
		if titleElement.Truthy() && nodeID(titleElement.Get("parentNode")) == nodeID(fragment) {
			titleElement.Call("remove")
			fragment.Set("title", titleElement.Get("innerText"))
		}
	}
	if fragment.Truthy() {
		if cfgBool("allowScriptTags") {
			normalizeScriptTags(fragment)
		} else {
			for _, s := range jsArrayToSlice(fragment.Call("querySelectorAll", "script")) {
				s.Call("remove")
			}
		}
	}
	return fragment
}

func maybeCall(fn func()) {
	if fn != nil {
		fn()
	}
}

func isRawObject(v js.Value) bool {
	return v.Type() == js.TypeObject &&
		global.Get("Object").Get("prototype").Get("toString").Call("call", v).String() == "[object Object]"
}

func isScrolledIntoView(el js.Value) bool {
	rect := el.Call("getBoundingClientRect")
	elemTop := rect.Get("top").Float()
	elemBottom := rect.Get("bottom").Float()
	return elemTop < global.Get("innerHeight").Float() && elemBottom >= 0
}

func bodyContains(elt js.Value) bool {
	opts := newObject()
	opts.Set("composed", true)
	return nodeID(elt.Call("getRootNode", opts)) == nodeID(getDocument())
}

var whitespaceSplitRe = regexp.MustCompile(`\s+`)

func splitOnWhitespace(trigger string) []string {
	return whitespaceSplitRe.Split(strings.TrimSpace(trigger), -1)
}

// mergeObjects copies obj2's own keys onto obj1 (JS objects).
func mergeObjects(obj1, obj2 js.Value) js.Value {
	if !obj2.Truthy() {
		return obj1
	}
	keys := global.Get("Object").Call("keys", obj2)
	for i := 0; i < keys.Get("length").Int(); i++ {
		k := keys.Index(i).String()
		obj1.Set(k, obj2.Get(k))
	}
	return obj1
}

func parseJSON(jString string) js.Value {
	// htmx.js calls JSON.parse on the RAW value. On the history/meta paths that
	// value is often the browser's `null` for an absent sessionStorage item or
	// meta tag — and JSON.parse(null) === null in JS (no throw, no log). Our jstr()
	// collapses that null to "" before it reaches here, so an empty string means
	// "nothing to parse", not "malformed JSON": return null silently, mirroring
	// htmx.js. JSON.parse("") would instead THROW, which spammed "htmx: invalid
	// JSON" to console.error on every first hx-boost/history navigation. Only a
	// non-empty, genuinely malformed string logs the error, as htmx.js does.
	if jString == "" {
		return null
	}
	out, ok := jsTry(global.Get("JSON"), "parse", jString)
	if !ok {
		logError("htmx: invalid JSON")
		return null
	}
	return out
}

func canAccessLocalStorage() bool {
	const test = "htmx:sessionStorageTest"
	// The property read is part of the probe: window.sessionStorage itself throws when
	// storage is blocked (sandboxed frame, cookies disabled), before any method call.
	ss, ok := jsTryGet(global, "sessionStorage")
	if !ok || !ss.Truthy() {
		return false
	}
	if _, ok := jsTry(ss, "setItem", test, test); !ok {
		return false
	}
	_, ok = jsTry(ss, "removeItem", test)
	return ok
}

var trailingSlashRe = regexp.MustCompile(`/+$`)

func normalizePath(path string) string {
	// new URL rejects a path that is not resolvable against the current href.
	if url, ok := jsTryNew(global.Get("URL"), path, location().Get("href")); ok {
		path = url.Get("pathname").String() + url.Get("search").String()
	}
	if path != "/" {
		path = trailingSlashRe.ReplaceAllString(path, "")
	}
	return path
}

func startsWith(str, prefix string) bool { return strings.HasPrefix(str, prefix) }
func endsWith(str, suffix string) bool   { return strings.HasSuffix(str, suffix) }

func contains(arr []string, s string) bool {
	for _, v := range arr {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Public API (DOM querying / manipulation)
// ---------------------------------------------------------------------------

func internalEval(str string) js.Value {
	var out js.Value = undefined
	maybeEval(getDocument().Get("body"), func() js.Value {
		out = global.Call("eval", str)
		return out
	}, undefined)
	return out
}

func onLoadHelper(callback js.Value) js.Value {
	listener := js.FuncOf(func(_ js.Value, args []js.Value) any {
		callback.Invoke(args[0].Get("detail").Get("elt"))
		return nil
	})
	addEventListenerImpl(js.ValueOf("htmx:load"), listener.Value, undefined, undefined)
	return listener.Value
}

func logAll() {
	htmxObj.Set("logger", js.FuncOf(func(_ js.Value, args []js.Value) any {
		global.Get("console").Call("log", args[1], args[0], args[2])
		return nil
	}))
}

func logNone() { htmxObj.Set("logger", null) }

// find(eltOrSelector, selector?) → Element|null
func find(eltOrSelector js.Value, selector string) js.Value {
	if eltOrSelector.Type() != js.TypeString {
		return eltOrSelector.Call("querySelector", selector)
	}
	return getDocument().Call("querySelector", eltOrSelector.String())
}

func findAll(eltOrSelector js.Value, selector string) js.Value {
	if eltOrSelector.Type() != js.TypeString {
		return eltOrSelector.Call("querySelectorAll", selector)
	}
	return getDocument().Call("querySelectorAll", eltOrSelector.String())
}

func removeElement(elt js.Value, delay float64) {
	elt = resolveTarget(elt, undefined)
	if delay > 0 {
		getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
			removeElement(elt, 0)
			return nil
		}), delay)
	} else {
		parentElt(elt).Call("removeChild", elt)
	}
}

func addClassToElement(elt js.Value, clazz string, delay float64) {
	elt = asElement(resolveTarget(elt, undefined))
	if !elt.Truthy() {
		return
	}
	if delay > 0 {
		getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
			addClassToElement(elt, clazz, 0)
			return nil
		}), delay)
	} else if elt.Get("classList").Truthy() {
		elt.Get("classList").Call("add", clazz)
	}
}

func removeClassFromElement(node js.Value, clazz string, delay float64) {
	elt := asElement(resolveTarget(node, undefined))
	if !elt.Truthy() {
		return
	}
	if delay > 0 {
		getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
			removeClassFromElement(elt, clazz, 0)
			return nil
		}), delay)
	} else if cl := elt.Get("classList"); cl.Truthy() {
		cl.Call("remove", clazz)
		if cl.Get("length").Int() == 0 {
			elt.Call("removeAttribute", "class")
		}
	}
}

func toggleClassOnElement(elt js.Value, clazz string) {
	elt = resolveTarget(elt, undefined)
	elt.Get("classList").Call("toggle", clazz)
}

func takeClassForElement(elt js.Value, clazz string) {
	elt = resolveTarget(elt, undefined)
	for _, child := range jsArrayToSlice(elt.Get("parentElement").Get("children")) {
		removeClassFromElement(child, clazz, 0)
	}
	addClassToElement(asElement(elt), clazz, 0)
}

func closest(elt js.Value, selector string) js.Value {
	elt = asElement(resolveTarget(elt, undefined))
	if elt.Truthy() {
		return elt.Call("closest", selector)
	}
	return null
}

func normalizeSelector(selector string) string {
	trimmed := strings.TrimSpace(selector)
	if startsWith(trimmed, "<") && endsWith(trimmed, "/>") {
		return trimmed[1 : len(trimmed)-2]
	}
	return trimmed
}

func asParentNode(elt js.Value) js.Value {
	if elt.Truthy() {
		nt := elt.Get("nodeType")
		if nt.Type() == js.TypeNumber {
			switch nt.Int() {
			case 1, 9, 11: // Element, Document, DocumentFragment
				return elt
			}
		}
	}
	return null
}

// querySelectorAllExt implements htmx's extended selector syntax.
func querySelectorAllExt(elt js.Value, selector string, global_ bool) []js.Value {
	if strings.Index(selector, "global ") == 0 {
		return querySelectorAllExt(elt, selector[7:], true)
	}
	elt = resolveTarget(elt, undefined)

	var parts []string
	{
		chevrons := 0
		offset := 0
		for i := 0; i < len(selector); i++ {
			ch := selector[i]
			if ch == ',' && chevrons == 0 {
				parts = append(parts, selector[offset:i])
				offset = i + 1
				continue
			}
			if ch == '<' {
				chevrons++
			} else if ch == '/' && i < len(selector)-1 && selector[i+1] == '>' {
				chevrons--
			}
		}
		if offset < len(selector) {
			parts = append(parts, selector[offset:])
		}
	}

	var result []js.Value
	var unprocessed []string
	for len(parts) > 0 {
		sel := normalizeSelector(parts[0])
		parts = parts[1:]
		var item js.Value = null
		switch {
		case strings.Index(sel, "closest ") == 0:
			item = closest(asElement(elt), normalizeSelector(sel[8:]))
		case strings.Index(sel, "find ") == 0:
			item = find(asParentNode(elt), normalizeSelector(sel[5:]))
		case sel == "next" || sel == "nextElementSibling":
			item = asElement(elt).Get("nextElementSibling")
		case strings.Index(sel, "next ") == 0:
			item = scanForwardQuery(elt, normalizeSelector(sel[5:]), global_)
		case sel == "previous" || sel == "previousElementSibling":
			item = asElement(elt).Get("previousElementSibling")
		case strings.Index(sel, "previous ") == 0:
			item = scanBackwardsQuery(elt, normalizeSelector(sel[9:]), global_)
		case sel == "document":
			item = getDocument()
		case sel == "window":
			item = global
		case sel == "body":
			item = getDocument().Get("body")
		case sel == "root":
			item = getRootNode(elt, global_)
		case sel == "host":
			item = elt.Call("getRootNode").Get("host")
		default:
			unprocessed = append(unprocessed, sel)
		}
		if item.Truthy() {
			result = append(result, item)
		}
	}

	if len(unprocessed) > 0 {
		standardSelector := strings.Join(unprocessed, ",")
		rootNode := asParentNode(getRootNode(elt, global_))
		result = append(result, jsArrayToSlice(rootNode.Call("querySelectorAll", standardSelector))...)
	}
	return result
}

func scanForwardQuery(start js.Value, match string, global_ bool) js.Value {
	results := asParentNode(getRootNode(start, global_)).Call("querySelectorAll", match)
	preceding := global.Get("Node").Get("DOCUMENT_POSITION_PRECEDING").Int()
	for i := 0; i < results.Get("length").Int(); i++ {
		elt := results.Index(i)
		if elt.Call("compareDocumentPosition", start).Int() == preceding {
			return elt
		}
	}
	return null
}

func scanBackwardsQuery(start js.Value, match string, global_ bool) js.Value {
	results := asParentNode(getRootNode(start, global_)).Call("querySelectorAll", match)
	following := global.Get("Node").Get("DOCUMENT_POSITION_FOLLOWING").Int()
	for i := results.Get("length").Int() - 1; i >= 0; i-- {
		elt := results.Index(i)
		if elt.Call("compareDocumentPosition", start).Int() == following {
			return elt
		}
	}
	return null
}

func querySelectorExt(eltOrSelector js.Value, selector string) js.Value {
	if eltOrSelector.Type() != js.TypeString {
		r := querySelectorAllExt(eltOrSelector, selector, false)
		if len(r) > 0 {
			return r[0]
		}
		return null
	}
	r := querySelectorAllExt(getDocument().Get("body"), eltOrSelector.String(), false)
	if len(r) > 0 {
		return r[0]
	}
	return null
}

// resolveTarget: a string is resolved with find(context||document, sel).
func resolveTarget(eltOrSelector, context js.Value) js.Value {
	if eltOrSelector.Type() == js.TypeString {
		root := asParentNode(context)
		if !root.Truthy() {
			root = getDocument()
		}
		return find(root, eltOrSelector.String())
	}
	return eltOrSelector
}

// addEventListenerImpl (htmx.on). arg1 may be a target or an event name.
//
// A string selector arg1 that resolves to no element yields a null target. In the
// browser htmx.js, null.addEventListener throws a JS TypeError synchronously — which
// a JS caller can try/catch and a Go caller of htmx.on via js.Value.Call catches as a
// recoverable panic in ITS OWN goroutine. Here the equivalent null-target .Call
// happens INSIDE this js.Func callback, where a panic cannot unwind back across the
// JS boundary to that caller's recover and instead terminates the whole Go runtime.
// So guard the null target: attaching a listener to a missing element is a no-op
// (there is nothing to attach to), matching the caller-observable outcome of the JS
// throw without the fatal crash.
func addEventListenerImpl(arg1, arg2, arg3, arg4 js.Value) js.Value {
	ready(func() {
		target, event, listener, options := processEventArgs(arg1, arg2, arg3, arg4)
		if !target.Truthy() {
			return
		}
		if options.Truthy() {
			target.Call("addEventListener", event, listener, options)
		} else {
			target.Call("addEventListener", event, listener)
		}
	})
	if isFunction(arg2) {
		return arg2
	}
	return arg3
}

// removeEventListenerImpl (htmx.off). The null-target guard is load-bearing: the
// common caller is a component teardown detaching a subtree-scoped listener whose
// scope-root element has ALREADY been removed from the DOM, so the selector resolves
// to null. null.removeEventListener would panic inside this callback and kill the
// runtime (see addEventListenerImpl for why that panic escapes the caller's recover).
// A removed element has already had its listeners dropped by the browser, so skipping
// the detach is correct.
func removeEventListenerImpl(arg1, arg2, arg3 js.Value) js.Value {
	ready(func() {
		target, event, listener, _ := processEventArgs(arg1, arg2, arg3, undefined)
		if !target.Truthy() {
			return
		}
		target.Call("removeEventListener", event, listener)
	})
	if isFunction(arg2) {
		return arg2
	}
	return arg3
}

func processEventArgs(arg1, arg2, arg3, arg4 js.Value) (target js.Value, event string, listener, options js.Value) {
	if isFunction(arg2) {
		return getDocument().Get("body"), jstr(arg1), arg2, arg3
	}
	return resolveTarget(arg1, undefined), jstr(arg2), arg3, arg4
}

// ---------------------------------------------------------------------------
// Node processing — targets, settling, OOB, swap primitives
// ---------------------------------------------------------------------------

// dummyElt for bad selectors (mirrors htmx DUMMY_ELT).
var dummyElt js.Value

func findAttributeTargets(elt js.Value, attrName string) []js.Value {
	attrTarget, ok := getClosestAttributeValue(elt, attrName)
	if !ok {
		return nil
	}
	if attrTarget == "this" {
		return []js.Value{findThisElement(elt, attrName)}
	}
	result := querySelectorAllExt(elt, attrTarget, false)
	if inheritRe.MatchString(attrTarget) {
		eltToInheritFrom := asElement(getClosestMatch(elt, func(parent js.Value) bool {
			return nodeID(parent) != nodeID(elt) && hasAttribute(asElement(parent), attrName)
		}))
		if eltToInheritFrom.Truthy() {
			result = append(result, findAttributeTargets(eltToInheritFrom, attrName)...)
		}
	}
	if len(result) == 0 {
		logError("The selector \"" + attrTarget + "\" on " + attrName + " returned no matches!")
		return []js.Value{dummyElt}
	}
	return result
}

var inheritRe = regexp.MustCompile(`(^|,)(\s*)inherit(\s*)($|,)`)

func findThisElement(elt js.Value, attribute string) js.Value {
	return asElement(getClosestMatch(elt, func(e js.Value) bool {
		return getAttributeValue(asElement(e), attribute) != ""
	}))
}

func getTarget(elt js.Value) js.Value {
	if targetStr, ok := getClosestAttributeValue(elt, "hx-target"); ok {
		if targetStr == "this" {
			return findThisElement(elt, "hx-target")
		}
		return querySelectorExt(elt, targetStr)
	}
	if getInternalData(elt).boosted {
		return getDocument().Get("body")
	}
	return elt
}

func shouldSettleAttribute(name string) bool {
	attrs := cfg("attributesToSettle")
	for i := 0; i < attrs.Get("length").Int(); i++ {
		if attrs.Index(i).String() == name {
			return true
		}
	}
	return false
}

func cloneAttributes(mergeTo, mergeFrom js.Value) {
	for _, attr := range jsArrayToSlice(mergeTo.Get("attributes")) {
		name := attr.Get("name").String()
		if !mergeFrom.Call("hasAttribute", name).Bool() && shouldSettleAttribute(name) {
			mergeTo.Call("removeAttribute", name)
		}
	}
	for _, attr := range jsArrayToSlice(mergeFrom.Get("attributes")) {
		name := attr.Get("name").String()
		if shouldSettleAttribute(name) {
			mergeTo.Call("setAttribute", name, attr.Get("value").String())
		}
	}
}

func isInlineSwap(swapStyle string, target js.Value) bool {
	for _, extension := range getExtensions(target, nil, nil) {
		// Third-party extension code: bridge the call, and read the result for
		// truthiness as htmx.js does — .Bool() on a non-boolean return would panic.
		if inline, ok := jsTry(extension, "isInlineSwap", swapStyle); ok && inline.Truthy() {
			return true
		}
	}
	return swapStyle == "outerHTML"
}

// settleInfo mirrors HtmxSettleInfo.
type settleInfo struct {
	tasks []func()
	elts  []js.Value
	title js.Value
}

func makeSettleInfo(target js.Value) *settleInfo {
	return &settleInfo{elts: []js.Value{target}, title: undefined}
}

// jsSettleInfo mirrors a settleInfo into the plain JS object an extension's
// handleSwap receives. A Go pointer cannot cross into JS, and extensions queue
// settle tasks on the object they are handed, so the mirror is folded back with
// mergeSettleInfo once the extension returns.
func jsSettleInfo(si *settleInfo) js.Value {
	mirror := newObject()
	mirror.Set("tasks", global.Get("Array").New())
	elts := global.Get("Array").New()
	for _, elt := range si.elts {
		elts.Call("push", elt)
	}
	mirror.Set("elts", elts)
	mirror.Set("title", si.title)
	return mirror
}

// mergeSettleInfo folds an extension's additions back into the Go settleInfo:
// the settle tasks it queued, the elements it appended, and a title it set.
func mergeSettleInfo(si *settleInfo, mirror js.Value) {
	if tasks := mirror.Get("tasks"); tasks.Truthy() {
		for i := 0; i < tasks.Length(); i++ {
			task := tasks.Index(i)
			si.tasks = append(si.tasks, func() { task.Invoke() })
		}
	}
	if elts := mirror.Get("elts"); elts.Truthy() {
		for i := len(si.elts); i < elts.Length(); i++ {
			si.elts = append(si.elts, elts.Index(i))
		}
	}
	if title := mirror.Get("title"); !title.Equal(si.title) {
		si.title = title
	}
}

func oobSwap(oobValue string, oobElement js.Value, si *settleInfo, rootNode js.Value) string {
	if !rootNode.Truthy() {
		rootNode = getDocument()
	}
	selector := "#" + global.Get("CSS").Call("escape", getRawAttribute(oobElement, "id")).String()
	swapStyle := "outerHTML"
	if oobValue == "true" {
		// do nothing
	} else if idx := strings.Index(oobValue, ":"); idx > 0 {
		swapStyle = oobValue[:idx]
		selector = oobValue[idx+1:]
	} else {
		swapStyle = oobValue
	}
	oobElement.Call("removeAttribute", "hx-swap-oob")
	oobElement.Call("removeAttribute", "data-hx-swap-oob")

	targets := querySelectorAllExt(rootNode, selector, false)
	if len(targets) > 0 {
		for _, target := range targets {
			oobElementClone := oobElement.Call("cloneNode", true)
			fragment := getDocument().Call("createDocumentFragment")
			fragment.Call("appendChild", oobElementClone)
			if !isInlineSwap(swapStyle, target) {
				fragment = asParentNode(oobElementClone)
			}
			details := newObject()
			details.Set("shouldSwap", true)
			details.Set("target", target)
			details.Set("fragment", fragment)
			if !triggerEvent(target, "htmx:oobBeforeSwap", details) {
				continue
			}
			target = details.Get("target")
			if details.Get("shouldSwap").Bool() {
				handlePreservedElements(fragment)
				swapWithStyle(swapStyle, target, target, fragment, si)
				restorePreservedElements()
			}
			for _, elt := range si.elts {
				triggerEvent(elt, "htmx:oobAfterSwap", details)
			}
		}
		oobElement.Get("parentNode").Call("removeChild", oobElement)
	} else {
		oobElement.Get("parentNode").Call("removeChild", oobElement)
		d := newObject()
		d.Set("content", oobElement)
		d.Set("target", selector)
		triggerErrorEvent(getDocument().Get("body"), "htmx:oobErrorNoTarget", d)
	}
	return oobValue
}

func restorePreservedElements() {
	pantry := find(js.ValueOf("#--htmx-preserve-pantry--"), "")
	if !pantry.Truthy() {
		return
	}
	for _, preservedElt := range jsArrayToSlice(pantry.Get("children")) {
		existingElement := find(js.ValueOf("#"+preservedElt.Get("id").String()), "")
		existingElement.Get("parentNode").Call("moveBefore", preservedElt, existingElement)
		existingElement.Call("remove")
	}
	pantry.Call("remove")
}

func handlePreservedElements(fragment js.Value) {
	for _, preservedElt := range jsArrayToSlice(findAll(fragment, "[hx-preserve], [data-hx-preserve]")) {
		id := getAttributeValue(preservedElt, "id")
		existingElement := getDocument().Call("getElementById", id)
		if !existingElement.Truthy() {
			continue
		}
		if preservedElt.Get("moveBefore").Truthy() {
			pantry := find(js.ValueOf("#--htmx-preserve-pantry--"), "")
			if !pantry.Truthy() {
				getDocument().Get("body").Call("insertAdjacentHTML", "afterend", "<div id='--htmx-preserve-pantry--'></div>")
				pantry = find(js.ValueOf("#--htmx-preserve-pantry--"), "")
			}
			pantry.Call("moveBefore", existingElement, null)
		} else {
			preservedElt.Get("parentNode").Call("replaceChild", existingElement, preservedElt)
		}
	}
}

func handleAttributes(parentNode, fragment js.Value, si *settleInfo) {
	for _, newNode := range jsArrayToSlice(fragment.Call("querySelectorAll", "[id]")) {
		id := getRawAttribute(newNode, "id")
		if id == "" {
			continue
		}
		normalizedID := strings.ReplaceAll(id, "'", "\\'")
		normalizedTag := strings.ReplaceAll(newNode.Get("tagName").String(), ":", "\\:")
		parentEl := asParentNode(parentNode)
		if !parentEl.Truthy() {
			continue
		}
		oldNode := parentEl.Call("querySelector", normalizedTag+"[id='"+normalizedID+"']")
		if oldNode.Truthy() && nodeID(oldNode) != nodeID(parentEl) {
			newAttributes := newNode.Call("cloneNode")
			cloneAttributes(newNode, oldNode)
			nn := newNode
			na := newAttributes
			si.tasks = append(si.tasks, func() { cloneAttributes(nn, na) })
		}
	}
}

func makeAjaxLoadTask(child js.Value) func() {
	return func() {
		removeClassFromElement(child, cfgStr("addedClass"), 0)
		processNode(asElement(child))
		processFocus(asParentNode(child))
		triggerEvent(child, "htmx:load", undefined)
	}
}

func processFocus(child js.Value) {
	const autofocus = "[autofocus]"
	var autoFocusedElt js.Value
	if matches(child, autofocus) {
		autoFocusedElt = child
	} else {
		autoFocusedElt = child.Call("querySelector", autofocus)
	}
	if autoFocusedElt.Truthy() {
		autoFocusedElt.Call("focus")
	}
}

func insertNodesBefore(parentNode, insertBefore, fragment js.Value, si *settleInfo) {
	handleAttributes(parentNode, fragment, si)
	textNode := global.Get("Node").Get("TEXT_NODE").Int()
	commentNode := global.Get("Node").Get("COMMENT_NODE").Int()
	for fragment.Get("childNodes").Get("length").Int() > 0 {
		child := fragment.Get("firstChild")
		addClassToElement(asElement(child), cfgStr("addedClass"), 0)
		parentNode.Call("insertBefore", child, insertBefore)
		if nt := child.Get("nodeType").Int(); nt != textNode && nt != commentNode {
			si.tasks = append(si.tasks, makeAjaxLoadTask(child))
		}
	}
}

func stringHash(s string, hash int32) int32 {
	for _, c := range s {
		hash = (hash << 5) - hash + int32(c)
	}
	return hash
}

func attributeHash(elt js.Value) int32 {
	var hash int32
	attrs := elt.Get("attributes")
	for i := 0; i < attrs.Get("length").Int(); i++ {
		attribute := attrs.Index(i)
		if attribute.Get("value").Truthy() {
			hash = stringHash(attribute.Get("name").String(), hash)
			hash = stringHash(attribute.Get("value").String(), hash)
		}
	}
	return hash
}

func deInitOnHandlers(elt js.Value) {
	d := getInternalData(elt)
	if d.onHandlers != nil {
		for _, h := range d.onHandlers {
			elt.Call("removeEventListener", h.event, h.listener)
			h.listener.Release()
		}
		d.onHandlers = nil
	}
}

func deInitNode(element js.Value) {
	d := getInternalData(element)
	if d.hasTimeout {
		global.Call("clearTimeout", d.timeout)
	}
	for _, info := range d.listenerInfos {
		if info.on.Truthy() {
			info.on.Call("removeEventListener", info.trigger, info.listener)
			info.listener.Release()
		}
	}
	deInitOnHandlers(element)
	// reset all data except firstInitCompleted
	first := d.firstInitCompleted
	*d = nodeData{lastButtonClicked: null, firstInitCompleted: first}
}

func cleanUpElement(element js.Value) {
	triggerEvent(element, "htmx:beforeCleanupElement", undefined)
	deInitNode(element)
	for _, child := range jsArrayToSlice(element.Get("children")) {
		cleanUpElement(child)
	}
}

func swapOuterHTML(target, fragment js.Value, si *settleInfo) {
	if target.Get("tagName").String() == "BODY" {
		swapInnerHTML(target, fragment, si)
		return
	}
	eltBeforeNewContent := target.Get("previousSibling")
	parentNode := parentElt(target)
	if !parentNode.Truthy() {
		return
	}
	insertNodesBefore(parentNode, target, fragment, si)
	var newElt js.Value
	if !eltBeforeNewContent.Truthy() {
		newElt = parentNode.Get("firstChild")
	} else {
		newElt = eltBeforeNewContent.Get("nextSibling")
	}
	si.elts = filterElts(si.elts, target)
	for newElt.Truthy() && nodeID(newElt) != nodeID(target) {
		if isElement(newElt) {
			si.elts = append(si.elts, newElt)
		}
		newElt = newElt.Get("nextSibling")
	}
	cleanUpElement(target)
	target.Call("remove")
}

func filterElts(elts []js.Value, remove js.Value) []js.Value {
	out := elts[:0:0]
	for _, e := range elts {
		if nodeID(e) != nodeID(remove) {
			out = append(out, e)
		}
	}
	return out
}

func swapAfterBegin(target, fragment js.Value, si *settleInfo) {
	insertNodesBefore(target, target.Get("firstChild"), fragment, si)
}
func swapBeforeBegin(target, fragment js.Value, si *settleInfo) {
	insertNodesBefore(parentElt(target), target, fragment, si)
}
func swapBeforeEnd(target, fragment js.Value, si *settleInfo) {
	insertNodesBefore(target, null, fragment, si)
}
func swapAfterEnd(target, fragment js.Value, si *settleInfo) {
	insertNodesBefore(parentElt(target), target.Get("nextSibling"), fragment, si)
}
func swapDelete(target js.Value) {
	cleanUpElement(target)
	if parent := parentElt(target); parent.Truthy() {
		parent.Call("removeChild", target)
	}
}

func swapInnerHTML(target, fragment js.Value, si *settleInfo) {
	firstChild := target.Get("firstChild")
	insertNodesBefore(target, firstChild, fragment, si)
	if firstChild.Truthy() {
		for firstChild.Get("nextSibling").Truthy() {
			cleanUpElement(firstChild.Get("nextSibling"))
			target.Call("removeChild", firstChild.Get("nextSibling"))
		}
		cleanUpElement(firstChild)
		target.Call("removeChild", firstChild)
	}
}

func swapWithStyle(swapStyle string, elt, target, fragment js.Value, si *settleInfo) {
	switch swapStyle {
	case "none":
		return
	case "outerHTML":
		swapOuterHTML(target, fragment, si)
		return
	case "afterbegin":
		swapAfterBegin(target, fragment, si)
		return
	case "beforebegin":
		swapBeforeBegin(target, fragment, si)
		return
	case "beforeend":
		swapBeforeEnd(target, fragment, si)
		return
	case "afterend":
		swapAfterEnd(target, fragment, si)
		return
	case "delete":
		swapDelete(target)
		return
	default:
		textNode := global.Get("Node").Get("TEXT_NODE").Int()
		commentNode := global.Get("Node").Get("COMMENT_NODE").Int()
		for _, ext := range getExtensions(elt, nil, nil) {
			mirror := jsSettleInfo(si)
			// A custom extension's handleSwap is arbitrary third-party JS; a throw from
			// it must leave the swap to fall through to the built-in styles below.
			newElements, handled := jsTry(ext, "handleSwap", swapStyle, target, fragment, mirror)
			mergeSettleInfo(si, mirror)
			if handled && newElements.Truthy() {
				if newElements.InstanceOf(global.Get("Array")) {
					for j := 0; j < newElements.Get("length").Int(); j++ {
						child := newElements.Index(j)
						if nt := child.Get("nodeType").Int(); nt != textNode && nt != commentNode {
							si.tasks = append(si.tasks, makeAjaxLoadTask(child))
						}
					}
				}
				return
			}
		}
		if swapStyle == "innerHTML" {
			swapInnerHTML(target, fragment, si)
		} else {
			swapWithStyle(cfgStr("defaultSwapStyle"), elt, target, fragment, si)
		}
	}
}

func findAndSwapOobElements(fragment js.Value, si *settleInfo, rootNode js.Value) bool {
	oobElts := jsArrayToSlice(findAll(fragment, "[hx-swap-oob], [data-hx-swap-oob]"))
	for _, oobElement := range oobElts {
		if cfgBool("allowNestedOobSwaps") || !oobElement.Get("parentElement").Truthy() {
			oobValue := getAttributeValue(oobElement, "hx-swap-oob")
			if oobValue != "" {
				oobSwap(oobValue, oobElement, si, rootNode)
			}
		} else {
			oobElement.Call("removeAttribute", "hx-swap-oob")
			oobElement.Call("removeAttribute", "data-hx-swap-oob")
		}
	}
	return len(oobElts) > 0
}

// swapOptions mirrors SwapOptions.
type swapOptions struct {
	selectStr           string
	selectOOB           string
	eventInfo           js.Value
	anchor              string
	contextElement      js.Value
	afterSwapCallback   func()
	afterSettleCallback func()
	beforeSwapCallback  func()
	title               js.Value
	historyRequest      bool
}

// swapSpecT mirrors HtmxSwapSpecification.
type swapSpecT struct {
	swapStyle     string
	swapDelay     float64
	settleDelay   float64
	transition    bool
	hasTransition bool
	ignoreTitle   bool
	hasIgnoreTitle bool
	scroll        string
	scrollTarget  string
	show          string
	showTarget    string
	focusScroll   bool
	hasFocusScroll bool
}

// swap implements the complete swapping pipeline.
func swap(target js.Value, content string, spec *swapSpecT, opts *swapOptions) {
	if opts == nil {
		opts = &swapOptions{eventInfo: undefined, title: undefined, contextElement: undefined}
	}
	if !opts.eventInfo.Truthy() {
		opts.eventInfo = undefined
	}

	doSwap := func() {
		maybeCall(opts.beforeSwapCallback)
		target = resolveTarget(target, undefined)
		var rootNode js.Value = getDocument()
		if opts.contextElement.Truthy() {
			rootNode = getRootNode(opts.contextElement, false)
		}

		activeElt := getDocument().Get("activeElement")
		selElt := activeElt
		var selStart, selEnd js.Value = null, null
		if activeElt.Truthy() {
			selStart = activeElt.Get("selectionStart")
			selEnd = activeElt.Get("selectionEnd")
		}
		si := makeSettleInfo(target)

		if spec.swapStyle == "textContent" {
			target.Set("textContent", content)
		} else {
			fragment := makeFragment(content)
			if opts.title.Truthy() {
				si.title = opts.title
			} else {
				si.title = fragment.Get("title")
			}
			if opts.historyRequest {
				if he := fragment.Call("querySelector", "[hx-history-elt],[data-hx-history-elt]"); he.Truthy() {
					fragment = he
				}
			}
			if opts.selectOOB != "" {
				for _, sel := range strings.Split(opts.selectOOB, ",") {
					pieces := strings.SplitN(sel, ":", 2)
					id := strings.TrimSpace(pieces[0])
					id = strings.TrimPrefix(id, "#")
					oobValue := "true"
					if len(pieces) > 1 {
						oobValue = pieces[1]
					}
					oobElement := fragment.Call("querySelector", "#"+id)
					if oobElement.Truthy() {
						oobSwap(oobValue, oobElement, si, rootNode)
					}
				}
			}
			findAndSwapOobElements(fragment, si, rootNode)
			for _, template := range jsArrayToSlice(findAll(fragment, "template")) {
				if template.Get("content").Truthy() && findAndSwapOobElements(template.Get("content"), si, rootNode) {
					template.Call("remove")
				}
			}
			if opts.selectStr != "" {
				newFragment := getDocument().Call("createDocumentFragment")
				for _, node := range jsArrayToSlice(fragment.Call("querySelectorAll", opts.selectStr)) {
					newFragment.Call("appendChild", node)
				}
				fragment = newFragment
			}
			handlePreservedElements(fragment)
			swapWithStyle(spec.swapStyle, opts.contextElement, target, fragment, si)
			restorePreservedElements()
		}

		if selElt.Truthy() && !bodyContains(selElt) && getRawAttribute(selElt, "id") != "" {
			newActiveElt := getDocument().Call("getElementById", getRawAttribute(selElt, "id"))
			preventScroll := !cfgBool("defaultFocusScroll")
			if spec.hasFocusScroll {
				preventScroll = !spec.focusScroll
			}
			focusOptions := newObject()
			focusOptions.Set("preventScroll", preventScroll)
			if newActiveElt.Truthy() {
				if selStart.Truthy() && newActiveElt.Get("setSelectionRange").Truthy() {
					// Throws InvalidStateError on an input type that does not support text
					// selection (email, number, date, ...); the exact set is per-spec and
					// per-browser, so let the call itself decide instead of guessing it.
					jsTry(newActiveElt, "setSelectionRange", selStart, selEnd)
				}
				newActiveElt.Call("focus", focusOptions)
			}
		}

		removeClassFromElement(target, cfgStr("swappingClass"), 0)
		for _, elt := range si.elts {
			if elt.Get("classList").Truthy() {
				elt.Get("classList").Call("add", cfgStr("settlingClass"))
			}
			triggerEvent(elt, "htmx:afterSwap", opts.eventInfo)
		}
		maybeCall(opts.afterSwapCallback)

		if !spec.ignoreTitle {
			handleTitle(jstr(si.title))
		}

		doSettle := func() {
			for _, task := range si.tasks {
				task()
			}
			for _, elt := range si.elts {
				if elt.Get("classList").Truthy() {
					removeClassFromElement(elt, cfgStr("settlingClass"), 0)
				}
				triggerEvent(elt, "htmx:afterSettle", opts.eventInfo)
			}
			if opts.anchor != "" {
				anchorTarget := asElement(resolveTarget(js.ValueOf("#"+opts.anchor), undefined))
				if anchorTarget.Truthy() {
					o := newObject()
					o.Set("block", "start")
					o.Set("behavior", "auto")
					anchorTarget.Call("scrollIntoView", o)
				}
			}
			updateScrollState(si.elts, spec)
			maybeCall(opts.afterSettleCallback)
		}

		if spec.settleDelay > 0 {
			getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any { doSettle(); return nil }), spec.settleDelay)
		} else {
			doSettle()
		}
	}

	// NOTE: view transitions (document.startViewTransition) are applied inline;
	// the Promise-wrapped settle path is folded into doSettle above.
	elt := getDocument()
	if opts.contextElement.Truthy() {
		elt = opts.contextElement
	}
	// doSwap is Go, not a single JS call, so the JS try/catch bridge cannot wrap it:
	// this is the one guard whose reach depends on the toolchain implementing recover.
	threw := safe(func() {
		if spec.swapDelay > 0 {
			getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any { doSwap(); return nil }), spec.swapDelay)
		} else {
			doSwap()
		}
	})
	if threw {
		triggerErrorEvent(elt, "htmx:swapError", opts.eventInfo)
	}
}

func handleTriggerHeader(xhr js.Value, header string, elt js.Value) {
	triggerBody := jstr(xhr.Call("getResponseHeader", header))
	if strings.Index(triggerBody, "{") == 0 {
		triggers := parseJSON(triggerBody)
		if !triggers.Truthy() {
			return
		}
		keys := global.Get("Object").Call("keys", triggers)
		for i := 0; i < keys.Get("length").Int(); i++ {
			eventName := keys.Index(i).String()
			detail := triggers.Get(eventName)
			target := elt
			if isRawObject(detail) {
				if detail.Get("target").Type() != js.TypeUndefined {
					target = detail.Get("target")
				}
			} else {
				d := newObject()
				d.Set("value", detail)
				detail = d
			}
			triggerEvent(target, eventName, detail)
		}
	} else {
		for _, eventName := range strings.Split(triggerBody, ",") {
			triggerEvent(elt, strings.TrimSpace(eventName), global.Get("Array").New())
		}
	}
}

// ---------------------------------------------------------------------------
// Trigger tokenizer + specification parsing
// ---------------------------------------------------------------------------

var (
	reSymbolStart      = regexp.MustCompile(`[_$a-zA-Z]`)
	reSymbolCont       = regexp.MustCompile(`[_$a-zA-Z0-9]`)
	reNotWhitespace    = regexp.MustCompile(`[^\s]`)
	reWhitespaceComma  = regexp.MustCompile(`[\s,]`)
	reCombinedSelStart = regexp.MustCompile(`[{(]`)
	reCombinedSelEnd   = regexp.MustCompile(`[})]`)
	reTriggerStop      = regexp.MustCompile(`[,\[\s]`)
)

var stringishStart = []string{`"`, `'`, `/`}

func charAt(s string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return string(s[i])
}

func tokenizeString(str string) []string {
	var tokens []string
	position := 0
	for position < len(str) {
		if reSymbolStart.MatchString(charAt(str, position)) {
			startPosition := position
			for reSymbolCont.MatchString(charAt(str, position+1)) {
				position++
			}
			tokens = append(tokens, str[startPosition:position+1])
		} else if contains(stringishStart, charAt(str, position)) {
			startChar := charAt(str, position)
			startPosition := position
			position++
			for position < len(str) && charAt(str, position) != startChar {
				if charAt(str, position) == "\\" {
					position++
				}
				position++
			}
			tokens = append(tokens, str[startPosition:min(position+1, len(str))])
		} else {
			tokens = append(tokens, charAt(str, position))
		}
		position++
	}
	return tokens
}

func isPossibleRelativeReference(token, last, paramName string) bool {
	return reSymbolStart.MatchString(charAt(token, 0)) &&
		token != "true" && token != "false" && token != "this" &&
		token != paramName && last != "."
}

// maybeGenerateConditional builds and evaluates a JS event-filter function,
// mirroring htmx (which uses new Function). Returns a JS function or null.
func maybeGenerateConditional(elt js.Value, tokens *[]string, paramName string) js.Value {
	if len(*tokens) == 0 || (*tokens)[0] != "[" {
		return null
	}
	*tokens = (*tokens)[1:]
	bracketCount := 1
	conditionalSource := " return (function(" + paramName + "){ return ("
	last := ""
	hadLast := false
	for len(*tokens) > 0 {
		token := (*tokens)[0]
		if token == "]" {
			bracketCount--
			if bracketCount == 0 {
				if !hadLast {
					conditionalSource += "true"
				}
				*tokens = (*tokens)[1:]
				conditionalSource += ")})"
				// The source comes from the element's hx-trigger, so both the Function
				// constructor (syntax error) and the outer invocation can throw.
				built := true
				fn := maybeEval(elt, func() js.Value {
					factory, ok := jsTryNew(global.Get("Function"), conditionalSource)
					if !ok {
						built = false
						return null
					}
					filter, ok := jsTry(factory, "call", undefined)
					built = ok
					return filter
				}, js.FuncOf(func(js.Value, []js.Value) any { return true }).Value)
				if !built {
					d := newObject()
					d.Set("source", conditionalSource)
					triggerErrorEvent(getDocument().Get("body"), "htmx:syntax:error", d)
					return null
				}
				if fn.Truthy() {
					fn.Set("source", conditionalSource)
				}
				return fn
			}
		} else if token == "[" {
			bracketCount++
		}
		if isPossibleRelativeReference(token, last, paramName) {
			conditionalSource += "((" + paramName + "." + token + ") ? (" + paramName + "." + token + ") : (window." + token + "))"
		} else {
			conditionalSource += token
		}
		last = (*tokens)[0]
		hadLast = true
		*tokens = (*tokens)[1:]
	}
	return null
}

func consumeUntil(tokens *[]string, match *regexp.Regexp) string {
	result := ""
	for len(*tokens) > 0 && !match.MatchString((*tokens)[0]) {
		result += (*tokens)[0]
		*tokens = (*tokens)[1:]
	}
	return result
}

func consumeCSSSelector(tokens *[]string) string {
	if len(*tokens) > 0 && reCombinedSelStart.MatchString((*tokens)[0]) {
		*tokens = (*tokens)[1:]
		result := strings.TrimSpace(consumeUntil(tokens, reCombinedSelEnd))
		if len(*tokens) > 0 {
			*tokens = (*tokens)[1:]
		}
		return result
	}
	return consumeUntil(tokens, reWhitespaceComma)
}

const inputSelector = "input, textarea, select"

// ---------------------------------------------------------------------------
// Trigger / publish event-name aliases (host-app bridge seam)
//
// A GENERIC extension point, empty by default, that is NOT part of upstream
// htmx.js: a consumer (e.g. the Gothic core runtime) can register a name PREFIX
// that htmx rewrites to a real DOM CustomEvent name. It is the seam that lets an
// hx-trigger / hx-publish value like "topic:cart" resolve to a host bus event
// such as "gothic:topic:cart" WITHOUT baking any host-specific event name into
// this port. With no alias registered every trigger/publish name passes through
// verbatim, so stock htmx behaviour is unchanged.
// ---------------------------------------------------------------------------

type triggerAlias struct {
	prefix      string
	eventPrefix string
	defaultFrom string
}

var triggerAliases []triggerAlias

// RegisterTriggerAlias registers a trigger/publish name alias. A trigger or an
// hx-publish value beginning with prefix is rewritten: prefix is replaced by
// eventPrefix (so "topic:cart" → "gothic:topic:cart") and — for a trigger that
// carries no explicit `from:` — the listener binds on defaultFrom (an htmx
// extended selector such as "document", where the host bus dispatches). Re-
// registering the same prefix replaces it. Call before Start() (or before the
// markup is processed) so the initial DOM pass sees the alias. Also exposed on
// window.htmx as htmx.registerTriggerAlias(prefix, eventPrefix, defaultFrom).
func RegisterTriggerAlias(prefix, eventPrefix, defaultFrom string) {
	if prefix == "" {
		return
	}
	for i := range triggerAliases {
		if triggerAliases[i].prefix == prefix {
			triggerAliases[i] = triggerAlias{prefix, eventPrefix, defaultFrom}
			return
		}
	}
	triggerAliases = append(triggerAliases, triggerAlias{prefix, eventPrefix, defaultFrom})
}

// resolveTriggerAlias returns the rewritten event name and default-from for name.
// ok is false when no registered prefix matches (name passes through unchanged).
func resolveTriggerAlias(name string) (event, from string, ok bool) {
	for _, a := range triggerAliases {
		if a.prefix != "" && strings.HasPrefix(name, a.prefix) {
			return a.eventPrefix + name[len(a.prefix):], a.defaultFrom, true
		}
	}
	return name, "", false
}

// triggerSpec mirrors HtmxTriggerSpecification.
type triggerSpec struct {
	trigger      string
	pollInterval float64
	eventFilter  js.Value
	changed      bool
	once         bool
	consume      bool
	delay        float64
	hasDelay     bool
	from         string
	target       string
	throttle     float64
	hasThrottle  bool
	queue        string
	hasQueue     bool
	root         string
	threshold    string
}

func newTriggerSpec(trigger string) *triggerSpec {
	return &triggerSpec{trigger: trigger, eventFilter: null}
}

func parseAndCacheTrigger(elt js.Value, explicitTrigger string) []*triggerSpec {
	var triggerSpecs []*triggerSpec
	tokens := tokenizeString(explicitTrigger)
	for {
		consumeUntil(&tokens, reNotWhitespace)
		initialLength := len(tokens)
		trigger := consumeUntil(&tokens, reTriggerStop)
		if trigger != "" {
			if trigger == "every" {
				every := newTriggerSpec("every")
				consumeUntil(&tokens, reNotWhitespace)
				if iv, ok := parseInterval(consumeUntil(&tokens, reTriggerStop)); ok {
					every.pollInterval = iv
				}
				consumeUntil(&tokens, reNotWhitespace)
				if ef := maybeGenerateConditional(elt, &tokens, "event"); ef.Truthy() {
					every.eventFilter = ef
				}
				triggerSpecs = append(triggerSpecs, every)
			} else {
				spec := newTriggerSpec(trigger)
				if ef := maybeGenerateConditional(elt, &tokens, "event"); ef.Truthy() {
					spec.eventFilter = ef
				}
				consumeUntil(&tokens, reNotWhitespace)
				for len(tokens) > 0 && tokens[0] != "," {
					token := tokens[0]
					tokens = tokens[1:]
					switch {
					case token == "changed":
						spec.changed = true
					case token == "once":
						spec.once = true
					case token == "consume":
						spec.consume = true
					case token == "delay" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						if iv, ok := parseInterval(consumeUntil(&tokens, reWhitespaceComma)); ok {
							spec.delay = iv
							spec.hasDelay = true
						}
					case token == "from" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						var fromArg string
						if reCombinedSelStart.MatchString(tokens[0]) {
							fromArg = consumeCSSSelector(&tokens)
						} else {
							fromArg = consumeUntil(&tokens, reWhitespaceComma)
							if fromArg == "closest" || fromArg == "find" || fromArg == "next" || fromArg == "previous" {
								tokens = tokens[1:]
								selector := consumeCSSSelector(&tokens)
								if len(selector) > 0 {
									fromArg += " " + selector
								}
							}
						}
						spec.from = fromArg
					case token == "target" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						spec.target = consumeCSSSelector(&tokens)
					case token == "throttle" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						if iv, ok := parseInterval(consumeUntil(&tokens, reWhitespaceComma)); ok {
							spec.throttle = iv
							spec.hasThrottle = true
						}
					case token == "queue" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						spec.queue = consumeUntil(&tokens, reWhitespaceComma)
						spec.hasQueue = true
					case token == "root" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						spec.root = consumeCSSSelector(&tokens)
					case token == "threshold" && len(tokens) > 0 && tokens[0] == ":":
						tokens = tokens[1:]
						spec.threshold = consumeUntil(&tokens, reWhitespaceComma)
					default:
						d := newObject()
						if len(tokens) > 0 {
							d.Set("token", tokens[0])
							tokens = tokens[1:]
						}
						triggerErrorEvent(elt, "htmx:syntax:error", d)
					}
					consumeUntil(&tokens, reNotWhitespace)
				}
				// Host-app bridge: rewrite an aliased trigger name (e.g.
				// "topic:cart" → "gothic:topic:cart") and, unless the author gave an
				// explicit `from:`, bind it on the alias's default source (e.g.
				// "document", where the host event bus dispatches). No-op when no
				// alias is registered.
				if ev, from, ok := resolveTriggerAlias(spec.trigger); ok {
					spec.trigger = ev
					if spec.from == "" && from != "" {
						spec.from = from
					}
				}
				triggerSpecs = append(triggerSpecs, spec)
			}
		}
		if len(tokens) == initialLength {
			d := newObject()
			if len(tokens) > 0 {
				d.Set("token", tokens[0])
				tokens = tokens[1:]
			}
			triggerErrorEvent(elt, "htmx:syntax:error", d)
		}
		consumeUntil(&tokens, reNotWhitespace)
		if len(tokens) > 0 && tokens[0] == "," {
			tokens = tokens[1:]
			continue
		}
		break
	}
	return triggerSpecs
}

func getTriggerSpecs(elt js.Value) []*triggerSpec {
	explicitTrigger := getAttributeValue(elt, "hx-trigger")
	var triggerSpecs []*triggerSpec
	if explicitTrigger != "" {
		triggerSpecs = parseAndCacheTrigger(elt, explicitTrigger)
	}
	if len(triggerSpecs) > 0 {
		return triggerSpecs
	}
	switch {
	case matches(elt, "form"):
		return []*triggerSpec{newTriggerSpec("submit")}
	case matches(elt, `input[type="button"], input[type="submit"]`):
		return []*triggerSpec{newTriggerSpec("click")}
	case matches(elt, inputSelector):
		return []*triggerSpec{newTriggerSpec("change")}
	default:
		return []*triggerSpec{newTriggerSpec("click")}
	}
}

// ---------------------------------------------------------------------------
// Trigger handling / boosting / event wiring
// ---------------------------------------------------------------------------

// triggerHandler is htmx's TriggerHandler callback (elt, evt).
type triggerHandler func(node, evt js.Value)

func cancelPolling(elt js.Value) { getInternalData(elt).cancelled = true }

func processPolling(elt js.Value, handler triggerHandler, spec *triggerSpec) {
	nodeData := getInternalData(elt)
	nodeData.timeout = getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
		if bodyContains(elt) && !nodeData.cancelled {
			evt := makeEvent("hx:poll:trigger", pollDetail(spec, elt))
			if !maybeFilterEvent(spec, elt, evt) {
				handler(elt, undefined)
			}
			processPolling(elt, handler, spec)
		}
		return nil
	}), spec.pollInterval)
	nodeData.hasTimeout = true
}

func pollDetail(spec *triggerSpec, elt js.Value) js.Value {
	d := newObject()
	d.Set("target", elt)
	return d
}

func isLocalLink(elt js.Value) bool {
	href := getRawAttribute(elt, "href")
	return location().Get("hostname").String() == elt.Get("hostname").String() &&
		href != "" && strings.Index(href, "#") != 0
}

func eltIsDisabled(elt js.Value) js.Value {
	return closest(elt, cfgStr("disableSelector"))
}

var reGetQuery = regexp.MustCompile(`\?[^#]+`)

func boostElement(elt js.Value, nodeData *nodeData, triggerSpecs []*triggerSpec) {
	isAnchor := elt.InstanceOf(global.Get("HTMLAnchorElement")) && isLocalLink(elt) &&
		(elt.Get("target").String() == "" || elt.Get("target").String() == "_self")
	isForm := elt.Get("tagName").String() == "FORM" && strings.ToLower(getRawAttribute(elt, "method")) != "dialog"
	if !isAnchor && !isForm {
		return
	}
	nodeData.boosted = true
	var verb, path string
	if elt.Get("tagName").String() == "A" {
		verb = "get"
		path = getRawAttribute(elt, "href")
	} else {
		rawMethod := getRawAttribute(elt, "method")
		if rawMethod != "" {
			verb = strings.ToLower(rawMethod)
		} else {
			verb = "get"
		}
		path = getRawAttribute(elt, "action")
		if path == "" {
			path = location().Get("href").String()
		}
		if verb == "get" && strings.Contains(path, "?") {
			path = reGetQuery.ReplaceAllString(path, "")
		}
	}
	for _, triggerSpec := range triggerSpecs {
		wireEventListener(elt, func(node, evt js.Value) {
			e := asElement(node)
			if eltIsDisabled(e).Truthy() {
				cleanUpElement(e)
				return
			}
			issueAjaxRequest(verb, path, e, evt, nil, false)
		}, nodeData, triggerSpec, true)
	}
}

var reSamePageAnchor = regexp.MustCompile(`^#.+`)

func shouldCancel(evt, elt js.Value) bool {
	if evt.Get("type").String() == "submit" && elt.Get("tagName").String() == "FORM" {
		return true
	}
	if evt.Get("type").String() == "click" {
		btn := elt.Call("closest", `input[type="submit"], button`)
		if btn.Truthy() && btn.Get("form").Truthy() && btn.Get("type").String() == "submit" {
			return true
		}
		link := elt.Call("closest", "a")
		if link.Truthy() && link.Get("href").Truthy() && !reSamePageAnchor.MatchString(getRawAttribute(link, "href")) {
			return true
		}
	}
	return false
}

func ignoreBoostedAnchorCtrlClick(elt, evt js.Value) bool {
	return getInternalData(elt).boosted && elt.InstanceOf(global.Get("HTMLAnchorElement")) &&
		evt.Get("type").String() == "click" &&
		(evt.Get("ctrlKey").Truthy() || evt.Get("metaKey").Truthy())
}

func maybeFilterEvent(spec *triggerSpec, elt, evt js.Value) bool {
	eventFilter := spec.eventFilter
	if eventFilter.Truthy() {
		// The filter body is a user-authored JS expression from hx-trigger.
		result, ok := jsTry(eventFilter, "call", elt, evt)
		if !ok {
			d := newObject()
			d.Set("source", eventFilter.Get("source"))
			triggerErrorEvent(getDocument().Get("body"), "htmx:eventFilter:error", d)
			return true
		}
		return result.Truthy() != true // filter passes when result !== true
	}
	return false
}

// wireEventListener is htmx's internal addEventListener(elt, handler, nodeData, spec, explicitCancel).
func wireEventListener(elt js.Value, handler triggerHandler, nodeData *nodeData, spec *triggerSpec, explicitCancel bool) {
	elementData := getInternalData(elt)
	var eltsToListenOn []js.Value
	if spec.from != "" {
		eltsToListenOn = querySelectorAllExt(elt, spec.from, false)
	} else {
		eltsToListenOn = []js.Value{elt}
	}
	if spec.changed {
		if elementData.lastValue == nil {
			elementData.lastValue = map[*triggerSpec]map[int]string{}
		}
		for _, eltToListenOn := range eltsToListenOn {
			if elementData.lastValue[spec] == nil {
				elementData.lastValue[spec] = map[int]string{}
			}
			elementData.lastValue[spec][nodeID(eltToListenOn)] = jstr(eltToListenOn.Get("value"))
		}
	}
	for _, eltToListenOn := range eltsToListenOn {
		eltToListenOn := eltToListenOn
		var eventListener js.Func
		eventListener = js.FuncOf(func(_ js.Value, args []js.Value) any {
			evt := args[0]
			if !bodyContains(elt) {
				eltToListenOn.Call("removeEventListener", spec.trigger, eventListener)
				return nil
			}
			if ignoreBoostedAnchorCtrlClick(elt, evt) {
				return nil
			}
			if explicitCancel || shouldCancel(evt, eltToListenOn) {
				evt.Call("preventDefault")
			}
			if maybeFilterEvent(spec, elt, evt) {
				return nil
			}
			eventData := getInternalData(evt)
			eventData.triggerSpec = spec
			if !containsID(eventData.handledFor, nodeID(elt)) {
				eventData.handledFor = append(eventData.handledFor, nodeID(elt))
				if spec.consume {
					evt.Call("stopPropagation")
				}
				if spec.target != "" && evt.Get("target").Truthy() {
					if !matches(asElement(evt.Get("target")), spec.target) {
						return nil
					}
				}
				if spec.once {
					if elementData.triggeredOnce {
						return nil
					}
					elementData.triggeredOnce = true
				}
				if spec.changed {
					node := evt.Get("target")
					value := jstr(node.Get("value"))
					lv := elementData.lastValue[spec]
					if lv != nil {
						if prev, ok := lv[nodeID(node)]; ok && prev == value {
							return nil
						}
						lv[nodeID(node)] = value
					}
				}
				if elementData.hasDelayed {
					global.Call("clearTimeout", elementData.delayed)
				}
				if elementData.hasThrottle {
					return nil
				}
				if spec.hasThrottle && spec.throttle > 0 {
					if !elementData.hasThrottle {
						triggerEvent(elt, "htmx:trigger", undefined)
						handler(elt, evt)
						elementData.throttle = getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
							elementData.hasThrottle = false
							return nil
						}), spec.throttle)
						elementData.hasThrottle = true
					}
				} else if spec.hasDelay && spec.delay > 0 {
					elementData.delayed = getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
						triggerEvent(elt, "htmx:trigger", undefined)
						handler(elt, evt)
						return nil
					}), spec.delay)
					elementData.hasDelayed = true
				} else {
					triggerEvent(elt, "htmx:trigger", undefined)
					handler(elt, evt)
				}
			}
			return nil
		})
		nodeData.listenerInfos = append(nodeData.listenerInfos, listenerInfo{
			trigger:  spec.trigger,
			listener: eventListener,
			on:       eltToListenOn,
		})
		eltToListenOn.Call("addEventListener", spec.trigger, eventListener)
	}
}

func containsID(ids []int, id int) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

var (
	windowIsScrolling = false
	scrollHandlerSet  = false
)

func initScrollHandler() {
	if scrollHandlerSet {
		return
	}
	scrollHandlerSet = true
	handler := js.FuncOf(func(js.Value, []js.Value) any { windowIsScrolling = true; return nil })
	global.Call("addEventListener", "scroll", handler)
	global.Call("addEventListener", "resize", handler)
	global.Call("setInterval", js.FuncOf(func(js.Value, []js.Value) any {
		if windowIsScrolling {
			windowIsScrolling = false
			for _, elt := range jsArrayToSlice(getDocument().Call("querySelectorAll", "[hx-trigger*='revealed'],[data-hx-trigger*='revealed']")) {
				maybeReveal(elt)
			}
		}
		return nil
	}), 200)
}

func maybeReveal(elt js.Value) {
	if hasAttribute(elt, "data-hx-revealed") || !isScrolledIntoView(elt) {
		return
	}
	elt.Call("setAttribute", "data-hx-revealed", "true")
	nodeData := getInternalData(elt)
	if nodeData.hasInitHash {
		triggerEvent(elt, "revealed", undefined)
	} else {
		opts := newObject()
		opts.Set("once", true)
		elt.Call("addEventListener", "htmx:afterProcessNode", js.FuncOf(func(js.Value, []js.Value) any {
			triggerEvent(elt, "revealed", undefined)
			return nil
		}), opts)
	}
}

func loadImmediately(elt js.Value, handler triggerHandler, nodeData *nodeData, delay float64) {
	load := func() {
		if !nodeData.loaded {
			nodeData.loaded = true
			triggerEvent(elt, "htmx:trigger", undefined)
			handler(elt, undefined)
		}
	}
	if delay > 0 {
		getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any { load(); return nil }), delay)
	} else {
		load()
	}
}

var verbsList = []string{"get", "post", "put", "delete", "patch"}

func processVerbs(elt js.Value, nodeData *nodeData, triggerSpecs []*triggerSpec) bool {
	explicitAction := false
	for _, verb := range verbsList {
		if hasAttribute(elt, "hx-"+verb) {
			path := getAttributeValue(elt, "hx-"+verb)
			explicitAction = true
			nodeData.path = path
			nodeData.verb = verb
			verb := verb
			for _, triggerSpec := range triggerSpecs {
				addTriggerHandler(elt, triggerSpec, nodeData, func(node, evt js.Value) {
					e := asElement(node)
					if eltIsDisabled(e).Truthy() {
						cleanUpElement(e)
						return
					}
					issueAjaxRequest(verb, path, e, evt, nil, false)
				})
			}
		}
	}
	return explicitAction
}

func addTriggerHandler(elt js.Value, spec *triggerSpec, nodeData *nodeData, handler triggerHandler) {
	switch {
	case spec.trigger == "revealed":
		initScrollHandler()
		wireEventListener(elt, handler, nodeData, spec, false)
		maybeReveal(asElement(elt))
	case spec.trigger == "intersect":
		observerOptions := newObject()
		if spec.root != "" {
			observerOptions.Set("root", querySelectorExt(elt, spec.root))
		}
		if spec.threshold != "" {
			if f, err := strconv.ParseFloat(spec.threshold, 64); err == nil {
				observerOptions.Set("threshold", f)
			}
		}
		observer := global.Get("IntersectionObserver").New(js.FuncOf(func(_ js.Value, args []js.Value) any {
			entries := args[0]
			for i := 0; i < entries.Get("length").Int(); i++ {
				if entries.Index(i).Get("isIntersecting").Bool() {
					triggerEvent(elt, "intersect", undefined)
					break
				}
			}
			return nil
		}), observerOptions)
		observer.Call("observe", asElement(elt))
		wireEventListener(asElement(elt), handler, nodeData, spec, false)
	case !nodeData.firstInitCompleted && spec.trigger == "load":
		if !maybeFilterEvent(spec, elt, makeEvent("load", loadDetail(elt))) {
			loadImmediately(asElement(elt), handler, nodeData, spec.delay)
		}
	case spec.pollInterval > 0:
		nodeData.polling = true
		processPolling(asElement(elt), handler, spec)
	default:
		wireEventListener(elt, handler, nodeData, spec, false)
	}
}

func loadDetail(elt js.Value) js.Value {
	d := newObject()
	d.Set("elt", elt)
	return d
}

func shouldProcessHxOn(node js.Value) bool {
	elt := asElement(node)
	if !elt.Truthy() {
		return false
	}
	attributes := elt.Get("attributes")
	for j := 0; j < attributes.Get("length").Int(); j++ {
		attrName := attributes.Index(j).Get("name").String()
		if startsWith(attrName, "hx-on:") || startsWith(attrName, "data-hx-on:") ||
			startsWith(attrName, "hx-on-") || startsWith(attrName, "data-hx-on-") {
			return true
		}
	}
	return false
}

var hxOnXPath js.Value

func processHXOnRoot(elt js.Value, elements *[]js.Value) {
	if shouldProcessHxOn(elt) {
		*elements = append(*elements, asElement(elt))
	}
	iter := hxOnXPath.Call("evaluate", elt)
	for {
		node := iter.Call("iterateNext")
		if !node.Truthy() {
			break
		}
		*elements = append(*elements, asElement(node))
	}
}

func findHxOnWildcardElements(elt js.Value) []js.Value {
	var elements []js.Value
	if elt.InstanceOf(global.Get("DocumentFragment")) {
		for _, child := range jsArrayToSlice(elt.Get("childNodes")) {
			processHXOnRoot(child, &elements)
		}
	} else {
		processHXOnRoot(elt, &elements)
	}
	return elements
}

func findElementsToProcess(elt js.Value) []js.Value {
	if !elt.Get("querySelectorAll").Truthy() {
		return nil
	}
	const boostedSelector = ", [hx-boost] a, [data-hx-boost] a, a[hx-boost], a[data-hx-boost]"
	var extensionSelectors []string
	for name := range extensions {
		ext := extensions[name]
		if ext.Get("getSelectors").Truthy() {
			selectors := ext.Call("getSelectors")
			if selectors.Truthy() {
				for _, s := range jsArrayToSlice(selectors) {
					extensionSelectors = append(extensionSelectors, s.String())
				}
			}
		}
	}
	sel := verbSelector + boostedSelector + ", form, [type='submit']," +
		" [hx-ext], [data-hx-ext], [hx-trigger], [data-hx-trigger]"
	for _, es := range extensionSelectors {
		sel += ", " + es
	}
	return jsArrayToSlice(elt.Call("querySelectorAll", sel))
}

func maybeSetLastButtonClicked(evt js.Value) {
	elt := getTargetButton(evt.Get("target"))
	internalData := getRelatedFormData(evt)
	if internalData != nil {
		internalData.lastButtonClicked = elt
	}
}

func maybeUnsetLastButtonClicked(evt js.Value) {
	internalData := getRelatedFormData(evt)
	if internalData != nil {
		internalData.lastButtonClicked = null
	}
}

func getTargetButton(target js.Value) js.Value {
	return closest(asElement(target), "button, input[type='submit']")
}

func getRelatedForm(elt js.Value) js.Value {
	if f := elt.Get("form"); f.Truthy() {
		return f
	}
	return closest(elt, "form")
}

func getRelatedFormData(evt js.Value) *nodeData {
	elt := getTargetButton(evt.Get("target"))
	if !elt.Truthy() {
		return nil
	}
	form := getRelatedForm(elt)
	if !form.Truthy() {
		return nil
	}
	return getInternalData(form)
}

func initButtonTracking(elt js.Value) {
	elt.Call("addEventListener", "click", js.FuncOf(func(_ js.Value, args []js.Value) any { maybeSetLastButtonClicked(args[0]); return nil }))
	elt.Call("addEventListener", "focusin", js.FuncOf(func(_ js.Value, args []js.Value) any { maybeSetLastButtonClicked(args[0]); return nil }))
	elt.Call("addEventListener", "focusout", js.FuncOf(func(_ js.Value, args []js.Value) any { maybeUnsetLastButtonClicked(args[0]); return nil }))
}

func addHxOnEventHandler(elt js.Value, eventName, code string) {
	nodeData := getInternalData(elt)
	var fn js.Value = null
	listener := js.FuncOf(func(_ js.Value, args []js.Value) any {
		e := args[0]
		maybeEval(elt, func() js.Value {
			if eltIsDisabled(elt).Truthy() {
				return undefined
			}
			if !fn.Truthy() {
				fn = global.Get("Function").New("event", code)
			}
			fn.Call("call", elt, e)
			return undefined
		}, undefined)
		return nil
	})
	elt.Call("addEventListener", eventName, listener)
	nodeData.onHandlers = append(nodeData.onHandlers, onHandler{event: eventName, listener: listener, target: elt})
}

func processHxOnWildcard(elt js.Value) {
	deInitOnHandlers(elt)
	attrs := elt.Get("attributes")
	for i := 0; i < attrs.Get("length").Int(); i++ {
		name := attrs.Index(i).Get("name").String()
		value := attrs.Index(i).Get("value").String()
		if startsWith(name, "hx-on") || startsWith(name, "data-hx-on") {
			afterOnPosition := strings.Index(name, "-on") + 3
			nextChar := ""
			if afterOnPosition < len(name) {
				nextChar = string(name[afterOnPosition])
			}
			if nextChar == "-" || nextChar == ":" {
				eventName := name[afterOnPosition+1:]
				switch {
				case startsWith(eventName, ":"):
					eventName = "htmx" + eventName
				case startsWith(eventName, "-"):
					eventName = "htmx:" + eventName[1:]
				case startsWith(eventName, "htmx-"):
					eventName = "htmx:" + eventName[5:]
				}
				addHxOnEventHandler(elt, eventName, value)
			}
		}
	}
}

func initNode(elt js.Value) {
	triggerEvent(elt, "htmx:beforeProcessNode", undefined)
	nodeData := getInternalData(elt)
	triggerSpecs := getTriggerSpecs(elt)
	hasExplicitHttpAction := processVerbs(elt, nodeData, triggerSpecs)
	if !hasExplicitHttpAction {
		if v, _ := getClosestAttributeValue(elt, "hx-boost"); v == "true" {
			boostElement(elt, nodeData, triggerSpecs)
		} else if hasAttribute(elt, "hx-trigger") {
			for _, triggerSpec := range triggerSpecs {
				addTriggerHandler(elt, triggerSpec, nodeData, func(js.Value, js.Value) {})
			}
		}
	}
	if elt.Get("tagName").String() == "FORM" ||
		(getRawAttribute(elt, "type") == "submit" && hasAttribute(elt, "form")) {
		initButtonTracking(elt)
	}
	nodeData.firstInitCompleted = true
	triggerEvent(elt, "htmx:afterProcessNode", undefined)
}

func maybeDeInitAndHash(elt js.Value) bool {
	if !isElement(elt) {
		return false
	}
	nodeData := getInternalData(elt)
	hash := attributeHash(elt)
	if !nodeData.hasInitHash || nodeData.initHash != hash {
		deInitNode(elt)
		nodeData = getInternalData(elt)
		nodeData.initHash = hash
		nodeData.hasInitHash = true
		return true
	}
	return false
}

func processNode(elt js.Value) {
	elt = resolveTarget(elt, undefined)
	if eltIsDisabled(elt).Truthy() {
		cleanUpElement(elt)
		return
	}
	var elementsToInit []js.Value
	if maybeDeInitAndHash(elt) {
		elementsToInit = append(elementsToInit, elt)
	}
	for _, child := range findElementsToProcess(elt) {
		if eltIsDisabled(child).Truthy() {
			cleanUpElement(child)
			continue
		}
		if maybeDeInitAndHash(child) {
			elementsToInit = append(elementsToInit, child)
		}
	}
	for _, e := range findHxOnWildcardElements(elt) {
		processHxOnWildcard(e)
	}
	for _, e := range elementsToInit {
		initNode(e)
	}
}

// verbSelector matches any element carrying an hx-VERB / data-hx-VERB attribute.
var verbSelector = func() string {
	var parts []string
	for _, verb := range verbsList {
		parts = append(parts, "[hx-"+verb+"], [data-hx-"+verb+"]")
	}
	return strings.Join(parts, ", ")
}()

// ---------------------------------------------------------------------------
// Event / Log support
// ---------------------------------------------------------------------------

var kebabRe = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func kebabEventName(str string) string {
	return strings.ToLower(kebabRe.ReplaceAllString(str, "$1-$2"))
}

func makeEvent(eventName string, detail js.Value) js.Value {
	init := newObject()
	init.Set("bubbles", true)
	init.Set("cancelable", true)
	init.Set("composed", true)
	if detail.Truthy() {
		init.Set("detail", detail)
	} else {
		init.Set("detail", newObject())
	}
	return global.Get("CustomEvent").New(eventName, init)
}

func triggerErrorEvent(elt js.Value, eventName string, detail js.Value) {
	base := newObject()
	base.Set("error", eventName)
	triggerEvent(elt, eventName, mergeObjects(base, detail))
}

func ignoreEventForLogging(eventName string) bool {
	return eventName == "htmx:afterProcessNode"
}

// withExtensions runs toDo for every extension registered for elt.
// Extension methods are third-party JS: toDo must reach them through jsTry so a
// throwing extension is skipped rather than taking the instance down with it.
func withExtensions(elt js.Value, toDo func(js.Value)) {
	for _, extension := range getExtensions(elt, nil, nil) {
		toDo(extension)
	}
}

func triggerEvent(elt js.Value, eventName string, detail js.Value) bool {
	elt = resolveTarget(elt, undefined)
	if !detail.Truthy() {
		detail = newObject()
	}
	detail.Set("elt", elt)
	event := makeEvent(eventName, detail)
	if logger := htmxObj.Get("logger"); logger.Truthy() && !ignoreEventForLogging(eventName) {
		logger.Invoke(elt, eventName, detail)
	}
	if detail.Get("error").Truthy() {
		logError(detail.Get("error"))
		ei := newObject()
		ei.Set("errorInfo", detail)
		triggerEvent(elt, "htmx:error", ei)
	}
	eventResult := elt.Call("dispatchEvent", event).Bool()
	kebabName := kebabEventName(eventName)
	if eventResult && kebabName != eventName {
		kebabedEvent := makeEvent(kebabName, event.Get("detail"))
		eventResult = eventResult && elt.Call("dispatchEvent", kebabedEvent).Bool()
	}
	withExtensions(asElement(elt), func(extension js.Value) {
		res, ok := jsTry(extension, "onEvent", eventName, event)
		if !ok {
			return
		}
		// htmx.js tests `extension.onEvent(...) != false`; an extension that
		// returns nothing yields undefined (here null), and `undefined != false`
		// is true. Calling .Bool() on a non-boolean would panic, so only treat an
		// explicit boolean false as a veto — everything else counts as "not false".
		resNotFalse := !(res.Type() == js.TypeBoolean && !res.Bool())
		eventResult = eventResult && resNotFalse && !event.Get("defaultPrevented").Bool()
	})
	return eventResult
}

// ---------------------------------------------------------------------------
// History support
// ---------------------------------------------------------------------------

var currentPathForHistory string

func setCurrentPathForHistory(path string) {
	currentPathForHistory = path
	if canAccessLocalStorage() {
		global.Get("sessionStorage").Call("setItem", "htmx-current-path-for-history", path)
	}
}

func getHistoryElement() js.Value {
	historyElt := getDocument().Call("querySelector", "[hx-history-elt],[data-hx-history-elt]")
	if historyElt.Truthy() {
		return historyElt
	}
	return getDocument().Get("body")
}

func saveToHistoryCache(url string, rootElt js.Value) {
	if !canAccessLocalStorage() {
		return
	}
	innerHTML := cleanInnerHtmlForHistory(rootElt)
	title := getDocument().Get("title").String()
	scroll := global.Get("scrollY")

	if cfgFloat("historyCacheSize") <= 0 {
		global.Get("sessionStorage").Call("removeItem", "htmx-history-cache")
		return
	}
	url = normalizePath(url)

	historyCache := parseJSON(jstr(global.Get("sessionStorage").Call("getItem", "htmx-history-cache")))
	if !historyCache.Truthy() {
		historyCache = global.Get("Array").New()
	}
	for i := 0; i < historyCache.Get("length").Int(); i++ {
		if historyCache.Index(i).Get("url").String() == url {
			historyCache.Call("splice", i, 1)
			break
		}
	}
	newHistoryItem := newObject()
	newHistoryItem.Set("url", url)
	newHistoryItem.Set("content", innerHTML)
	newHistoryItem.Set("title", title)
	newHistoryItem.Set("scroll", scroll)

	d := newObject()
	d.Set("item", newHistoryItem)
	d.Set("cache", historyCache)
	triggerEvent(getDocument().Get("body"), "htmx:historyItemCreated", d)

	historyCache.Call("push", newHistoryItem)
	for historyCache.Get("length").Int() > int(cfgFloat("historyCacheSize")) {
		historyCache.Call("shift")
	}
	for historyCache.Get("length").Int() > 0 {
		// Both steps can fail on their own: an htmx:historyItemCreated listener may have
		// left the cache unserializable, and setItem throws once the quota is exceeded.
		// Either way the loop drops the oldest entry and retries.
		if payload, encoded := jsTry(global.Get("JSON"), "stringify", historyCache); encoded {
			if _, stored := jsTry(global.Get("sessionStorage"), "setItem", "htmx-history-cache", payload); stored {
				break
			}
		}
		ce := newObject()
		ce.Set("cache", historyCache)
		triggerErrorEvent(getDocument().Get("body"), "htmx:historyCacheError", ce)
		historyCache.Call("shift")
	}
}

func getCachedHistory(url string) js.Value {
	if !canAccessLocalStorage() {
		return null
	}
	url = normalizePath(url)
	historyCache := parseJSON(jstr(global.Get("sessionStorage").Call("getItem", "htmx-history-cache")))
	if !historyCache.Truthy() {
		return null
	}
	for i := 0; i < historyCache.Get("length").Int(); i++ {
		if historyCache.Index(i).Get("url").String() == url {
			return historyCache.Index(i)
		}
	}
	return null
}

func cleanInnerHtmlForHistory(elt js.Value) string {
	className := cfgStr("requestClass")
	clone := elt.Call("cloneNode", true)
	for _, child := range jsArrayToSlice(findAll(clone, "."+className)) {
		removeClassFromElement(child, className, 0)
	}
	for _, child := range jsArrayToSlice(findAll(clone, "[data-disabled-by-htmx]")) {
		child.Call("removeAttribute", "disabled")
	}
	return clone.Get("innerHTML").String()
}

func saveCurrentPageToHistory() {
	elt := getHistoryElement()
	path := currentPathForHistory
	if canAccessLocalStorage() {
		path = jstr(global.Get("sessionStorage").Call("getItem", "htmx-current-path-for-history"))
	}
	if path == "" {
		path = location().Get("pathname").String() + location().Get("search").String()
	}
	disableHistoryCache := getDocument().Call("querySelector", `[hx-history="false" i],[data-hx-history="false" i]`)
	if !disableHistoryCache.Truthy() {
		d := newObject()
		d.Set("path", path)
		d.Set("historyElt", elt)
		triggerEvent(getDocument().Get("body"), "htmx:beforeHistorySave", d)
		saveToHistoryCache(path, elt)
	}
	if cfgBool("historyEnabled") {
		st := newObject()
		st.Set("htmx", true)
		global.Get("history").Call("replaceState", st, getDocument().Get("title"), location().Get("href"))
	}
}

var reCacheBuster = regexp.MustCompile(`org\.htmx\.cache-buster=[^&]*&?`)

func pushUrlIntoHistory(path string) {
	if cfgBool("getCacheBusterParam") {
		path = reCacheBuster.ReplaceAllString(path, "")
		if endsWith(path, "&") || endsWith(path, "?") {
			path = path[:len(path)-1]
		}
	}
	if cfgBool("historyEnabled") {
		st := newObject()
		st.Set("htmx", true)
		global.Get("history").Call("pushState", st, "", path)
	}
	setCurrentPathForHistory(path)
}

func replaceUrlInHistory(path string) {
	if cfgBool("historyEnabled") {
		st := newObject()
		st.Set("htmx", true)
		global.Get("history").Call("replaceState", st, "", path)
	}
	setCurrentPathForHistory(path)
}

func settleImmediately(tasks []func()) {
	for _, task := range tasks {
		task()
	}
}

func loadHistoryFromServer(path string) {
	request := global.Get("XMLHttpRequest").New()
	spec := &swapSpecT{swapStyle: "innerHTML", swapDelay: 0, settleDelay: 0}
	historyElt := getHistoryElement()
	details := newObject()
	details.Set("path", path)
	details.Set("xhr", request)
	details.Set("historyElt", historyElt)
	request.Call("open", "GET", path, true)
	if cfgBool("historyRestoreAsHxRequest") {
		request.Call("setRequestHeader", "HX-Request", "true")
	}
	request.Call("setRequestHeader", "HX-History-Restore-Request", "true")
	request.Call("setRequestHeader", "HX-Current-URL", location().Get("href"))
	request.Set("onload", js.FuncOf(func(this js.Value, _ []js.Value) any {
		status := this.Get("status").Int()
		if status >= 200 && status < 400 {
			response := this.Get("response")
			details.Set("response", response)
			triggerEvent(getDocument().Get("body"), "htmx:historyCacheMissLoad", details)
			swap(historyElt, jstr(response), spec, &swapOptions{
				contextElement: historyElt,
				historyRequest: true,
				title:          undefined,
				eventInfo:      undefined,
			})
			setCurrentPathForHistory(path)
			hr := newObject()
			hr.Set("path", path)
			hr.Set("cacheMiss", true)
			hr.Set("serverResponse", response)
			triggerEvent(getDocument().Get("body"), "htmx:historyRestore", hr)
		} else {
			triggerErrorEvent(getDocument().Get("body"), "htmx:historyCacheMissLoadError", details)
		}
		return nil
	}))
	if triggerEvent(getDocument().Get("body"), "htmx:historyCacheMiss", details) {
		request.Call("send")
	}
}

func restoreHistory(path string) {
	saveCurrentPageToHistory()
	if path == "" {
		path = location().Get("pathname").String() + location().Get("search").String()
	}
	cached := getCachedHistory(path)
	if cached.Truthy() {
		spec := &swapSpecT{swapStyle: "innerHTML", swapDelay: 0, settleDelay: 0}
		historyElt := getHistoryElement()
		details := newObject()
		details.Set("path", path)
		details.Set("item", cached)
		details.Set("historyElt", historyElt)
		if triggerEvent(getDocument().Get("body"), "htmx:historyCacheHit", details) {
			swap(historyElt, cached.Get("content").String(), spec, &swapOptions{
				contextElement: historyElt,
				title:          cached.Get("title"),
				eventInfo:      undefined,
			})
			setCurrentPathForHistory(path)
			triggerEvent(getDocument().Get("body"), "htmx:historyRestore", details)
		}
	} else if cfgBool("refreshOnHistoryMiss") {
		global.Get("location").Call("reload", true)
	} else {
		loadHistoryFromServer(path)
	}
}

// ---------------------------------------------------------------------------
// Request indicators
// ---------------------------------------------------------------------------

func addRequestIndicatorClasses(elt js.Value) []js.Value {
	indicators := findAttributeTargets(elt, "hx-indicator")
	if indicators == nil {
		indicators = []js.Value{elt}
	}
	for _, ic := range indicators {
		internalData := getInternalData(ic)
		internalData.requestCount++
		ic.Get("classList").Call("add", cfgStr("requestClass"))
	}
	return indicators
}

func disableElements(elt js.Value) []js.Value {
	disabledElts := findAttributeTargets(elt, "hx-disabled-elt")
	if disabledElts == nil {
		disabledElts = nil
	}
	for _, disabledElement := range disabledElts {
		internalData := getInternalData(disabledElement)
		internalData.requestCount++
		if !disabledElement.Call("hasAttribute", "disabled").Bool() {
			disabledElement.Call("setAttribute", "disabled", "")
			disabledElement.Call("setAttribute", "data-disabled-by-htmx", "")
		}
	}
	return disabledElts
}

func removeRequestIndicators(indicators, disabled []js.Value) {
	for _, ele := range append(append([]js.Value{}, indicators...), disabled...) {
		internalData := getInternalData(ele)
		if internalData.requestCount == 0 {
			internalData.requestCount = 1
		}
		internalData.requestCount--
	}
	for _, ic := range indicators {
		if getInternalData(ic).requestCount == 0 {
			removeClassFromElement(ic, cfgStr("requestClass"), 0)
		}
	}
	for _, disabledElement := range disabled {
		if getInternalData(disabledElement).requestCount == 0 && disabledElement.Call("hasAttribute", "data-disabled-by-htmx").Bool() {
			disabledElement.Call("removeAttribute", "disabled")
			disabledElement.Call("removeAttribute", "data-disabled-by-htmx")
		}
	}
}

// ---------------------------------------------------------------------------
// Input value processing
// ---------------------------------------------------------------------------

func haveSeenNode(processed []js.Value, elt js.Value) bool {
	for _, node := range processed {
		if node.Call("isSameNode", elt).Bool() {
			return true
		}
	}
	return false
}

func shouldInclude(element js.Value) bool {
	elt := element
	name := jstr(elt.Get("name"))
	if name == "" || elt.Get("disabled").Truthy() || closest(elt, "fieldset[disabled]").Truthy() {
		return false
	}
	typ := jstr(elt.Get("type"))
	tag := jstr(elt.Get("tagName"))
	if typ == "button" || typ == "submit" || tag == "image" || tag == "reset" || tag == "file" {
		return false
	}
	if typ == "checkbox" || typ == "radio" {
		return elt.Get("checked").Bool()
	}
	return true
}

func addValueToFormData(name string, value js.Value, formData js.Value) {
	if name == "" || !value.Truthy() && value.Type() != js.TypeString {
		if name == "" {
			return
		}
	}
	if value.InstanceOf(global.Get("Array")) {
		for i := 0; i < value.Get("length").Int(); i++ {
			formData.Call("append", name, value.Index(i))
		}
	} else {
		formData.Call("append", name, value)
	}
}

func removeValueFromFormData(name string, value js.Value, formData js.Value) {
	if name == "" {
		return
	}
	values := formData.Call("getAll", name)
	var kept []js.Value
	isArr := value.InstanceOf(global.Get("Array"))
	for i := 0; i < values.Get("length").Int(); i++ {
		v := values.Index(i)
		if isArr {
			if !jsArrayIncludes(value, v) {
				kept = append(kept, v)
			}
		} else if !v.Equal(value) {
			kept = append(kept, v)
		}
	}
	formData.Call("delete", name)
	for _, v := range kept {
		formData.Call("append", name, v)
	}
}

func jsArrayIncludes(arr, v js.Value) bool {
	return arr.Call("indexOf", v).Int() >= 0
}

func getValueFromInput(elt js.Value) js.Value {
	if elt.InstanceOf(global.Get("HTMLSelectElement")) && elt.Get("multiple").Truthy() {
		arr := global.Get("Array").New()
		for _, e := range jsArrayToSlice(elt.Call("querySelectorAll", "option:checked")) {
			arr.Call("push", e.Get("value"))
		}
		return arr
	}
	if elt.InstanceOf(global.Get("HTMLInputElement")) && elt.Get("files").Truthy() {
		return global.Get("Array").Call("from", elt.Get("files"))
	}
	return elt.Get("value")
}

func processInputValue(processed *[]js.Value, formData js.Value, errors *[]js.Value, elt js.Value, validate bool) {
	if !elt.Truthy() || haveSeenNode(*processed, elt) {
		return
	}
	*processed = append(*processed, elt)
	if shouldInclude(elt) {
		name := getRawAttribute(elt, "name")
		addValueToFormData(name, getValueFromInput(elt), formData)
		if validate {
			validateElement(elt, errors)
		}
	}
	if elt.InstanceOf(global.Get("HTMLFormElement")) {
		for _, input := range jsArrayToSlice(elt.Get("elements")) {
			if haveSeenNode(*processed, input) {
				removeValueFromFormData(jstr(input.Get("name")), getValueFromInput(input), formData)
			} else {
				*processed = append(*processed, input)
			}
			if validate {
				validateElement(input, errors)
			}
		}
		fd := global.Get("FormData").New(elt)
		iterFormData(fd, func(value js.Value, name string) {
			if value.InstanceOf(global.Get("File")) && jstr(value.Get("name")) == "" {
				return
			}
			addValueToFormData(name, value, formData)
		})
	}
}

func iterFormData(fd js.Value, fn func(value js.Value, name string)) {
	it := fd.Call("entries")
	for {
		r := it.Call("next")
		if r.Get("done").Bool() {
			break
		}
		pair := r.Get("value")
		fn(pair.Index(1), pair.Index(0).String())
	}
}

func validateElement(elt js.Value, errors *[]js.Value) {
	element := elt
	if !element.Get("willValidate").Truthy() {
		return
	}
	triggerEvent(element, "htmx:validation:validate", undefined)
	if !element.Call("checkValidity").Bool() {
		d := newObject()
		d.Set("message", element.Get("validationMessage"))
		d.Set("validity", element.Get("validity"))
		if triggerEvent(element, "htmx:validation:failed", d) && len(*errors) == 0 && cfgBool("reportValidityOfForms") {
			element.Call("reportValidity")
		}
		e := newObject()
		e.Set("elt", element)
		e.Set("message", element.Get("validationMessage"))
		e.Set("validity", element.Get("validity"))
		*errors = append(*errors, e)
	}
}

func overrideFormData(receiver, donor js.Value) js.Value {
	keys := donor.Call("keys")
	for {
		r := keys.Call("next")
		if r.Get("done").Bool() {
			break
		}
		receiver.Call("delete", r.Get("value"))
	}
	iterFormData(donor, func(value js.Value, key string) {
		receiver.Call("append", key, value)
	})
	return receiver
}

func getInputValues(elt js.Value, verb string) (errors []js.Value, formData, values js.Value) {
	processed := []js.Value{}
	formData = global.Get("FormData").New()
	priorityFormData := global.Get("FormData").New()
	internalData := getInternalData(elt)
	if internalData.lastButtonClicked.Truthy() && !bodyContains(internalData.lastButtonClicked) {
		internalData.lastButtonClicked = null
	}
	validate := (elt.InstanceOf(global.Get("HTMLFormElement")) && elt.Get("noValidate").Bool() != true) ||
		getAttributeValue(elt, "hx-validate") == "true"
	if internalData.lastButtonClicked.Truthy() {
		validate = validate && internalData.lastButtonClicked.Get("formNoValidate").Bool() != true
	}
	if verb != "get" {
		processInputValue(&processed, priorityFormData, &errors, getRelatedForm(elt), validate)
	}
	processInputValue(&processed, formData, &errors, elt, validate)
	if internalData.lastButtonClicked.Truthy() || jstr(elt.Get("tagName")) == "BUTTON" ||
		(jstr(elt.Get("tagName")) == "INPUT" && getRawAttribute(elt, "type") == "submit") {
		button := internalData.lastButtonClicked
		if !button.Truthy() {
			button = elt
		}
		addValueToFormData(getRawAttribute(button, "name"), button.Get("value"), priorityFormData)
	}
	for _, node := range findAttributeTargets(elt, "hx-include") {
		processInputValue(&processed, formData, &errors, asElement(node), validate)
		if !matches(node, "form") {
			for _, descendant := range jsArrayToSlice(asParentNode(node).Call("querySelectorAll", inputSelector)) {
				processInputValue(&processed, formData, &errors, descendant, validate)
			}
		}
	}
	overrideFormData(formData, priorityFormData)
	return errors, formData, formDataProxy(formData)
}

func appendParam(returnStr, name string, realValue js.Value) string {
	if returnStr != "" {
		returnStr += "&"
	}
	var s string
	if realValue.Type() == js.TypeObject && !realValue.InstanceOf(global.Get("Blob")) && !realValue.InstanceOf(global.Get("File")) {
		s = global.Get("encodeURIComponent").Invoke(global.Get("JSON").Call("stringify", realValue)).String()
	} else {
		s = global.Get("encodeURIComponent").Invoke(realValue).String()
	}
	returnStr += global.Get("encodeURIComponent").Invoke(name).String() + "=" + s
	return returnStr
}

func urlEncode(values js.Value) string {
	values = formDataFromObject(values)
	returnStr := ""
	iterFormData(values, func(value js.Value, key string) {
		returnStr = appendParam(returnStr, key, value)
	})
	return returnStr
}

// ---------------------------------------------------------------------------
// Ajax — headers, swap spec, expression vars
// ---------------------------------------------------------------------------

func getHeaders(elt, target js.Value, prompt js.Value) js.Value {
	headers := newObject()
	headers.Set("HX-Request", "true")
	setOrNull(headers, "HX-Trigger", getRawAttribute(elt, "id"))
	setOrNull(headers, "HX-Trigger-Name", getRawAttribute(elt, "name"))
	setOrNull(headers, "HX-Target", getAttributeValue(target, "id"))
	headers.Set("HX-Current-URL", location().Get("href"))
	getValuesForElement(elt, "hx-headers", false, headers, undefined)
	if prompt.Type() != js.TypeUndefined {
		headers.Set("HX-Prompt", prompt)
	}
	if getInternalData(elt).boosted {
		headers.Set("HX-Boosted", "true")
	}
	return headers
}

func setOrNull(obj js.Value, key, val string) {
	if val == "" {
		obj.Set(key, null)
	} else {
		obj.Set(key, val)
	}
}

func filterValues(inputValues, elt js.Value) js.Value {
	paramsValue, ok := getClosestAttributeValue(elt, "hx-params")
	if !ok {
		return inputValues
	}
	switch {
	case paramsValue == "none":
		return global.Get("FormData").New()
	case paramsValue == "*":
		return inputValues
	case strings.Index(paramsValue, "not ") == 0:
		for _, name := range strings.Split(paramsValue[4:], ",") {
			inputValues.Call("delete", strings.TrimSpace(name))
		}
		return inputValues
	default:
		newValues := global.Get("FormData").New()
		for _, name := range strings.Split(paramsValue, ",") {
			name = strings.TrimSpace(name)
			if inputValues.Call("has", name).Bool() {
				all := inputValues.Call("getAll", name)
				for i := 0; i < all.Get("length").Int(); i++ {
					newValues.Call("append", name, all.Index(i))
				}
			}
		}
		return newValues
	}
}

func isAnchorLink(elt js.Value) bool {
	href := getRawAttribute(elt, "href")
	return href != "" && strings.Index(href, "#") >= 0
}

func getSwapSpecification(elt js.Value, swapInfoOverride string, hasOverride bool) *swapSpecT {
	swapInfo := swapInfoOverride
	if !hasOverride {
		swapInfo, _ = getClosestAttributeValue(elt, "hx-swap")
	}
	style := cfgStr("defaultSwapStyle")
	if getInternalData(elt).boosted {
		style = "innerHTML"
	}
	spec := &swapSpecT{
		swapStyle:   style,
		swapDelay:   cfgFloat("defaultSwapDelay"),
		settleDelay: cfgFloat("defaultSettleDelay"),
	}
	if cfgBool("scrollIntoViewOnBoost") && getInternalData(elt).boosted && !isAnchorLink(elt) {
		spec.show = "top"
	}
	if swapInfo != "" {
		split := splitOnWhitespace(swapInfo)
		for i, value := range split {
			switch {
			case strings.Index(value, "swap:") == 0:
				if iv, ok := parseInterval(value[5:]); ok {
					spec.swapDelay = iv
				}
			case strings.Index(value, "settle:") == 0:
				if iv, ok := parseInterval(value[7:]); ok {
					spec.settleDelay = iv
				}
			case strings.Index(value, "transition:") == 0:
				spec.transition = value[11:] == "true"
				spec.hasTransition = true
			case strings.Index(value, "ignoreTitle:") == 0:
				spec.ignoreTitle = value[12:] == "true"
				spec.hasIgnoreTitle = true
			case strings.Index(value, "scroll:") == 0:
				parts := strings.Split(value[7:], ":")
				scrollVal := parts[len(parts)-1]
				if len(parts) > 1 {
					spec.scrollTarget = strings.Join(parts[:len(parts)-1], ":")
				}
				spec.scroll = scrollVal
			case strings.Index(value, "show:") == 0:
				parts := strings.Split(value[5:], ":")
				showVal := parts[len(parts)-1]
				if len(parts) > 1 {
					spec.showTarget = strings.Join(parts[:len(parts)-1], ":")
				}
				spec.show = showVal
			case strings.Index(value, "focus-scroll:") == 0:
				spec.focusScroll = value[len("focus-scroll:"):] == "true"
				spec.hasFocusScroll = true
			case i == 0:
				spec.swapStyle = value
			default:
				logError("Unknown modifier in hx-swap: " + value)
			}
		}
	}
	return spec
}

func usesFormData(elt js.Value) bool {
	if v, _ := getClosestAttributeValue(elt, "hx-encoding"); v == "multipart/form-data" {
		return true
	}
	return matches(elt, "form") && getRawAttribute(elt, "enctype") == "multipart/form-data"
}

func encodeParamsForBody(xhr, elt, filteredParameters js.Value) js.Value {
	var encoded js.Value = null
	withExtensions(elt, func(extension js.Value) {
		if !encoded.Truthy() {
			if v, ok := jsTry(extension, "encodeParameters", xhr, filteredParameters, elt); ok {
				encoded = v
			}
		}
	})
	if encoded.Truthy() {
		return encoded
	}
	if usesFormData(elt) {
		return overrideFormData(global.Get("FormData").New(), formDataFromObject(filteredParameters))
	}
	return js.ValueOf(urlEncode(filteredParameters))
}

func updateScrollState(content []js.Value, spec *swapSpecT) {
	if len(content) == 0 {
		return
	}
	first := content[0]
	last := content[len(content)-1]
	if spec.scroll != "" {
		var target js.Value = null
		if spec.scrollTarget != "" {
			target = asElement(querySelectorExt(first, spec.scrollTarget))
		}
		if spec.scroll == "top" && (first.Truthy() || target.Truthy()) {
			if !target.Truthy() {
				target = first
			}
			target.Set("scrollTop", 0)
		}
		if spec.scroll == "bottom" && (last.Truthy() || target.Truthy()) {
			if !target.Truthy() {
				target = last
			}
			target.Set("scrollTop", target.Get("scrollHeight"))
		}
		if n, err := strconv.Atoi(spec.scroll); err == nil {
			getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
				global.Call("scrollTo", 0, n)
				return nil
			}), 0)
		}
	}
	if spec.show != "" {
		var target js.Value = null
		if spec.showTarget != "" {
			targetStr := spec.showTarget
			if targetStr == "window" {
				targetStr = "body"
			}
			target = asElement(querySelectorExt(first, targetStr))
		}
		if spec.show == "top" && (first.Truthy() || target.Truthy()) {
			if !target.Truthy() {
				target = first
			}
			o := newObject()
			o.Set("block", "start")
			o.Set("behavior", cfgStr("scrollBehavior"))
			target.Call("scrollIntoView", o)
		}
		if spec.show == "bottom" && (last.Truthy() || target.Truthy()) {
			if !target.Truthy() {
				target = last
			}
			o := newObject()
			o.Set("block", "end")
			o.Set("behavior", cfgStr("scrollBehavior"))
			target.Call("scrollIntoView", o)
		}
	}
}

// getValuesForElement walks ancestry collecting attr JSON/js: values into values.
func getValuesForElement(elt js.Value, attr string, evalAsDefault bool, values, event js.Value) js.Value {
	if !values.Truthy() {
		values = newObject()
	}
	if !elt.Truthy() {
		return values
	}
	attributeValue := getAttributeValue(elt, attr)
	if attributeValue != "" {
		str := strings.TrimSpace(attributeValue)
		evaluateValue := evalAsDefault
		if str == "unset" {
			return null
		}
		if strings.Index(str, "javascript:") == 0 {
			str = str[11:]
			evaluateValue = true
		} else if strings.Index(str, "js:") == 0 {
			str = str[3:]
			evaluateValue = true
		}
		if strings.Index(str, "{") != 0 {
			str = "{" + str + "}"
		}
		var varsValues js.Value
		if evaluateValue {
			src := str
			varsValues = maybeEval(elt, func() js.Value {
				if event.Truthy() {
					return global.Get("Function").New("event", "return ("+src+")").Call("call", elt, event)
				}
				return global.Get("Function").New("return ("+src+")").Call("call", elt)
			}, newObject())
		} else {
			varsValues = parseJSON(str)
		}
		if varsValues.Truthy() {
			keys := global.Get("Object").Call("keys", varsValues)
			for i := 0; i < keys.Get("length").Int(); i++ {
				key := keys.Index(i).String()
				if !values.Get(key).Truthy() {
					values.Set(key, varsValues.Get(key))
				}
			}
		}
	}
	return getValuesForElement(asElement(parentElt(elt)), attr, evalAsDefault, values, event)
}

func maybeEval(elt js.Value, toEval func() js.Value, defaultVal js.Value) js.Value {
	if cfgBool("allowEval") {
		return toEval()
	}
	triggerErrorEvent(elt, "htmx:evalDisallowedError", undefined)
	return defaultVal
}

func getHXVarsForElement(elt, event, expressionVars js.Value) js.Value {
	return getValuesForElement(elt, "hx-vars", true, expressionVars, event)
}

func getHXValsForElement(elt, event, expressionVars js.Value) js.Value {
	return getValuesForElement(elt, "hx-vals", false, expressionVars, event)
}

func getExpressionVars(elt, event js.Value) js.Value {
	return mergeObjects(getHXVarsForElement(elt, event, undefined), getHXValsForElement(elt, event, undefined))
}

func safelySetHeaderValue(xhr js.Value, header string, headerValue js.Value) {
	if headerValue.Type() == js.TypeNull {
		return
	}
	// setRequestHeader rejects a value with characters outside the HTTP field grammar;
	// the retry sends it URI-encoded. A value that survives neither form is dropped,
	// since letting the second throw propagate would take the whole request path down.
	if _, ok := jsTry(xhr, "setRequestHeader", header, headerValue); ok {
		return
	}
	encoded, ok := jsTry(global, "encodeURIComponent", headerValue)
	if !ok {
		logError("htmx: unencodable header value dropped: " + header)
		return
	}
	if _, ok := jsTry(xhr, "setRequestHeader", header, encoded); !ok {
		logError("htmx: rejected header dropped: " + header)
		return
	}
	jsTry(xhr, "setRequestHeader", header+"-URI-AutoEncoded", "true")
}

func getPathFromResponse(xhr js.Value) string {
	if xhr.Get("responseURL").Truthy() {
		// new URL rejects a malformed responseURL.
		url, ok := jsTryNew(global.Get("URL"), xhr.Get("responseURL"))
		if !ok {
			d := newObject()
			d.Set("url", xhr.Get("responseURL"))
			triggerErrorEvent(getDocument().Get("body"), "htmx:badResponseUrl", d)
			return ""
		}
		return url.Get("pathname").String() + url.Get("search").String()
	}
	return ""
}

func hasHeader(xhr js.Value, regexpSource string) bool {
	re := global.Get("RegExp").New(regexpSource, "i")
	return re.Call("test", xhr.Call("getAllResponseHeaders")).Bool()
}

func hierarchyForElt(elt js.Value) []js.Value {
	var arr []js.Value
	for elt.Truthy() {
		arr = append(arr, elt)
		elt = elt.Get("parentElement")
	}
	return arr
}

func verifyPath(elt js.Value, path string, requestConfig js.Value) bool {
	var url js.Value
	base := window_origin()
	if location().Get("protocol").String() != "about:" {
		base = location().Get("href").String()
	}
	url = global.Get("URL").New(path, base)
	origin := window_origin()
	if location().Get("protocol").String() != "about:" {
		origin = location().Get("origin").String()
	}
	sameHost := origin == url.Get("origin").String()
	if cfgBool("selfRequestsOnly") && !sameHost {
		return false
	}
	d := newObject()
	d.Set("url", url)
	d.Set("sameHost", sameHost)
	return triggerEvent(elt, "htmx:validateUrl", mergeObjects(d, requestConfig))
}

func window_origin() string { return global.Get("origin").String() }

func formDataFromObject(obj js.Value) js.Value {
	if obj.InstanceOf(global.Get("FormData")) {
		return obj
	}
	formData := global.Get("FormData").New()
	// htmx.js iterates `for (const key in obj)` — only objects/functions carry
	// enumerable own keys worth appending. A primitive (string/number/bool) or
	// null/undefined reaching here is a non-object values arg (e.g. an unparsed
	// hx-vals string or a mutated requestConfig.parameters); JS would harmlessly
	// yield no meaningful FormData, and js.Value.Get on it PANICS, so bail early.
	if !isObjectOrFunction(obj) {
		return formData
	}
	keys := global.Get("Object").Call("keys", obj)
	for i := 0; i < keys.Get("length").Int(); i++ {
		key := keys.Index(i).String()
		val := obj.Get(key)
		// htmx.js: `if (obj[key] && typeof obj[key].forEach === 'function')`.
		// `.forEach` member access on a string/number in JS is a harmless
		// `undefined`; in Go js.Value.Get PANICS on a non-object, so the value
		// must be an object/function before probing for a forEach method.
		if val.Truthy() && isObjectOrFunction(val) && val.Get("forEach").Type() == js.TypeFunction {
			val.Call("forEach", js.FuncOf(func(_ js.Value, a []js.Value) any {
				formData.Call("append", key, a[0])
				return nil
			}))
		} else if val.Type() == js.TypeObject && !val.InstanceOf(global.Get("Blob")) {
			formData.Call("append", key, global.Get("JSON").Call("stringify", val))
		} else {
			formData.Call("append", key, val)
		}
	}
	return formData
}

func toJSArray(a []js.Value) js.Value {
	arr := global.Get("Array").New()
	for _, v := range a {
		arr.Call("push", v)
	}
	return arr
}

func formDataArrayProxy(formData js.Value, name string, array js.Value) js.Value {
	handler := newObject()
	handler.Set("get", js.FuncOf(func(_ js.Value, args []js.Value) any {
		target := args[0]
		key := args[1]
		if key.Type() == js.TypeString {
			switch key.String() {
			case "length":
				return target.Get("length")
			case "push":
				return js.FuncOf(func(_ js.Value, a []js.Value) any {
					target.Call("push", a[0])
					formData.Call("append", name, a[0])
					return nil
				})
			}
			if target.Get(key.String()).Type() == js.TypeFunction {
				return js.FuncOf(func(_ js.Value, a []js.Value) any {
					r := target.Get(key.String()).Call("apply", target, toJSArray(a))
					formData.Call("delete", name)
					target.Call("forEach", js.FuncOf(func(_ js.Value, b []js.Value) any {
						formData.Call("append", name, b[0])
						return nil
					}))
					return r
				})
			}
		}
		v := target.Get(strings.TrimSpace(jstr(key)))
		if key.Type() == js.TypeString {
			v = target.Get(key.String())
		}
		// htmx.js: `if (target[key] && target[key].length === 1) return target[key][0]`.
		// A FormData value element is typically a string; `"abc".length` is a harmless
		// read in JS but js.Value.Get("length") PANICS on a Go string value. Gate the
		// length probe on object/function type: for a plain string this returns v
		// unchanged, which is behaviourally identical to htmx.js (a 1-char string's
		// [0] is the same char; longer strings fall through to returning the string).
		if v.Truthy() && isObjectOrFunction(v) && v.Get("length").Int() == 1 {
			return v.Index(0)
		}
		return v
	}))
	handler.Set("set", js.FuncOf(func(_ js.Value, args []js.Value) any {
		target := args[0]
		index := args[1]
		value := args[2]
		target.SetIndex(index.Int(), value)
		formData.Call("delete", name)
		target.Call("forEach", js.FuncOf(func(_ js.Value, b []js.Value) any {
			formData.Call("append", name, b[0])
			return nil
		}))
		return true
	}))
	return global.Get("Proxy").New(array, handler)
}

func formDataProxy(formData js.Value) js.Value {
	handler := newObject()
	handler.Set("get", js.FuncOf(func(_ js.Value, args []js.Value) any {
		target := args[0]
		name := args[1]
		if name.Type() == js.TypeSymbol {
			result := global.Get("Reflect").Call("get", target, name)
			if result.Type() == js.TypeFunction {
				return js.FuncOf(func(_ js.Value, a []js.Value) any {
					return result.Call("apply", formData, toJSArray(a))
				})
			}
			return result
		}
		nameStr := name.String()
		if nameStr == "toJSON" {
			return js.FuncOf(func(js.Value, []js.Value) any {
				return global.Get("Object").Call("fromEntries", formData)
			})
		}
		if target.Get(nameStr).Type() == js.TypeFunction {
			return js.FuncOf(func(_ js.Value, a []js.Value) any {
				return formData.Get(nameStr).Call("apply", formData, toJSArray(a))
			})
		}
		array := formData.Call("getAll", name)
		n := array.Get("length").Int()
		if n == 0 {
			return undefined
		}
		if n == 1 {
			return array.Index(0)
		}
		return formDataArrayProxy(target, nameStr, array)
	}))
	handler.Set("set", js.FuncOf(func(_ js.Value, args []js.Value) any {
		target := args[0]
		name := args[1]
		value := args[2]
		if name.Type() != js.TypeString {
			return false
		}
		target.Call("delete", name.String())
		// htmx.js probes `typeof value.forEach === 'function'`; on a string (or
		// any non-object) `value.forEach` is a harmless undefined. In Go,
		// js.Value.Get panics unless value is an object/function, so gate the
		// forEach probe on the value's type first — otherwise assigning a plain
		// string to detail.parameters[key] (the request-signer contract) panics
		// the runtime instead of appending the string to the body.
		if value.Truthy() && isObjectOrFunction(value) && value.Get("forEach").Type() == js.TypeFunction {
			value.Call("forEach", js.FuncOf(func(_ js.Value, a []js.Value) any {
				target.Call("append", name.String(), a[0])
				return nil
			}))
		} else if value.Type() == js.TypeObject && !value.InstanceOf(global.Get("Blob")) {
			target.Call("append", name.String(), global.Get("JSON").Call("stringify", value))
		} else {
			target.Call("append", name.String(), value)
		}
		return true
	}))
	handler.Set("deleteProperty", js.FuncOf(func(_ js.Value, args []js.Value) any {
		name := args[1]
		if name.Type() == js.TypeString {
			args[0].Call("delete", name.String())
		}
		return true
	}))
	handler.Set("ownKeys", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return global.Get("Reflect").Call("ownKeys", global.Get("Object").Call("fromEntries", args[0]))
	}))
	handler.Set("getOwnPropertyDescriptor", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return global.Get("Reflect").Call("getOwnPropertyDescriptor", global.Get("Object").Call("fromEntries", args[0]), args[1])
	}))
	return global.Get("Proxy").New(formData, handler)
}

// ---------------------------------------------------------------------------
// Ajax — issueAjaxRequest (XHR) + response handling
// ---------------------------------------------------------------------------

// ajaxEtc mirrors HtmxAjaxEtc.
type ajaxEtc struct {
	returnPromise   bool
	handler         js.Value
	selectStr       string
	hasSelect       bool
	targetOverride  js.Value
	swapOverride    string
	hasSwapOverride bool
	headers         js.Value
	values          js.Value
	credentials     js.Value
	hasCredentials  bool
	timeout         js.Value
	hasTimeout      bool
	push            string
	hasPush         bool
	replace         string
	hasReplace      bool
	selectOOB       string
	hasSelectOOB    bool
	event           js.Value
}

func newEtc() *ajaxEtc {
	return &ajaxEtc{handler: null, targetOverride: null, headers: null, values: null,
		credentials: undefined, timeout: undefined, event: undefined}
}

func ajaxHelper(verb, path string, context js.Value) js.Value {
	verb = strings.ToLower(verb)
	if !context.Truthy() {
		etc := newEtc()
		etc.returnPromise = true
		return issueAjaxRequest(verb, path, null, undefined, etc, false)
	}
	if context.InstanceOf(global.Get("Element")) || context.Type() == js.TypeString {
		etc := newEtc()
		etc.returnPromise = true
		tgt := resolveTarget(context, undefined)
		if !tgt.Truthy() {
			tgt = dummyElt
		}
		etc.targetOverride = tgt
		return issueAjaxRequest(verb, path, null, undefined, etc, false)
	}
	resolvedTarget := resolveTarget(context.Get("target"), undefined)
	if (context.Get("target").Truthy() && !resolvedTarget.Truthy()) ||
		(context.Get("source").Truthy() && !resolvedTarget.Truthy() && !resolveTarget(context.Get("source"), undefined).Truthy()) {
		resolvedTarget = dummyElt
	}
	etc := newEtc()
	etc.returnPromise = true
	etc.targetOverride = resolvedTarget
	if h := context.Get("handler"); h.Truthy() {
		etc.handler = h
	}
	if h := context.Get("headers"); h.Truthy() {
		etc.headers = h
	}
	if v := context.Get("values"); v.Truthy() {
		etc.values = v
	}
	if s := context.Get("swap"); s.Truthy() {
		etc.swapOverride = s.String()
		etc.hasSwapOverride = true
	}
	if s := context.Get("select"); s.Truthy() {
		etc.selectStr = s.String()
		etc.hasSelect = true
	}
	if p := context.Get("push"); p.Truthy() {
		etc.push = p.String()
		etc.hasPush = true
	}
	if r := context.Get("replace"); r.Truthy() {
		etc.replace = r.String()
		etc.hasReplace = true
	}
	if o := context.Get("selectOOB"); o.Truthy() {
		etc.selectOOB = o.String()
		etc.hasSelectOOB = true
	}
	return issueAjaxRequest(verb, path, resolveTarget(context.Get("source"), undefined), context.Get("event"), etc, false)
}

func issueAjaxRequest(verb, path string, elt, event js.Value, etc *ajaxEtc, confirmed bool) js.Value {
	if etc == nil {
		etc = newEtc()
	}
	var promise js.Value = undefined
	var resolveFn, rejectFn js.Value = undefined, undefined
	if etc.returnPromise {
		promise = global.Get("Promise").New(js.FuncOf(func(_ js.Value, a []js.Value) any {
			resolveFn = a[0]
			rejectFn = a[1]
			return nil
		}))
	}
	resolve := func() {
		if resolveFn.Truthy() {
			resolveFn.Invoke()
		}
	}
	reject := func() {
		if rejectFn.Truthy() {
			rejectFn.Invoke()
		}
	}
	if !elt.Truthy() {
		elt = getDocument().Get("body")
	}
	selectStr := ""
	if etc.hasSelect {
		selectStr = etc.selectStr
	}

	if !bodyContains(elt) {
		resolve()
		return promise
	}
	target := etc.targetOverride
	if !target.Truthy() {
		target = asElement(getTarget(elt))
	}
	if !target.Truthy() || target.Equal(dummyElt) {
		d := newObject()
		if t, _ := getClosestAttributeValue(elt, "hx-target"); t != "" {
			d.Set("target", t)
		} else {
			d.Set("target", null)
		}
		triggerErrorEvent(elt, "htmx:targetError", d)
		reject()
		return promise
	}

	eltData := getInternalData(elt)
	submitter := eltData.lastButtonClicked
	if submitter.Truthy() {
		if buttonPath := getRawAttribute(submitter, "formaction"); buttonPath != "" {
			path = buttonPath
		}
		if buttonVerb := getRawAttribute(submitter, "formmethod"); buttonVerb != "" {
			if contains(verbsList, strings.ToLower(buttonVerb)) {
				verb = strings.ToLower(buttonVerb)
			} else {
				resolve()
				return promise
			}
		}
	}

	confirmQuestion, _ := getClosestAttributeValue(elt, "hx-confirm")
	if !confirmed {
		issueRequest := js.FuncOf(func(_ js.Value, a []js.Value) any {
			skip := false
			if len(a) > 0 {
				skip = a[0].Truthy()
			}
			issueAjaxRequest(verb, path, elt, event, etc, skip)
			return nil
		})
		confirmDetails := newObject()
		confirmDetails.Set("target", target)
		confirmDetails.Set("elt", elt)
		confirmDetails.Set("path", path)
		confirmDetails.Set("verb", verb)
		confirmDetails.Set("triggeringEvent", event)
		confirmDetails.Set("issueRequest", issueRequest)
		confirmDetails.Set("question", confirmQuestion)
		if !triggerEvent(elt, "htmx:confirm", confirmDetails) {
			resolve()
			return promise
		}
	}

	syncElt := elt
	syncStrategy, _ := getClosestAttributeValue(elt, "hx-sync")
	queueStrategy := ""
	hasQueueStrategy := false
	abortable := false
	if syncStrategy != "" {
		syncStrings := strings.Split(syncStrategy, ":")
		selector := strings.TrimSpace(syncStrings[0])
		if selector == "this" {
			syncElt = findThisElement(elt, "hx-sync")
		} else {
			syncElt = asElement(querySelectorExt(elt, selector))
		}
		syncStrategy = "drop"
		if len(syncStrings) > 1 {
			syncStrategy = strings.TrimSpace(syncStrings[1])
		}
		eltData = getInternalData(syncElt)
		switch {
		case syncStrategy == "drop" && eltData.hasXhr && !eltData.abortable:
			resolve()
			return promise
		case syncStrategy == "abort":
			if eltData.hasXhr {
				resolve()
				return promise
			}
			abortable = true
		case syncStrategy == "replace":
			triggerEvent(syncElt, "htmx:abort", undefined)
		case strings.Index(syncStrategy, "queue") == 0:
			queueStrArray := strings.Split(syncStrategy, " ")
			queueStrategy = "last"
			if len(queueStrArray) > 1 {
				queueStrategy = strings.TrimSpace(queueStrArray[1])
			}
			hasQueueStrategy = true
		}
	}

	if eltData.hasXhr {
		if eltData.abortable {
			triggerEvent(syncElt, "htmx:abort", undefined)
		} else {
			if !hasQueueStrategy {
				if event.Truthy() {
					eventData := getInternalData(event)
					if eventData.triggerSpec != nil && eventData.triggerSpec.hasQueue {
						queueStrategy = eventData.triggerSpec.queue
						hasQueueStrategy = true
					}
				}
				if !hasQueueStrategy {
					queueStrategy = "last"
				}
			}
			enqueue := func() { issueAjaxRequest(verb, path, elt, event, etc, false) }
			switch {
			case queueStrategy == "first" && len(eltData.queuedRequests) == 0:
				eltData.queuedRequests = append(eltData.queuedRequests, enqueue)
			case queueStrategy == "all":
				eltData.queuedRequests = append(eltData.queuedRequests, enqueue)
			case queueStrategy == "last":
				eltData.queuedRequests = []func(){enqueue}
			}
			resolve()
			return promise
		}
	}

	xhr := global.Get("XMLHttpRequest").New()
	eltData.xhr = xhr
	eltData.hasXhr = true
	eltData.abortable = abortable
	endRequestLock := func() {
		eltData.xhr = null
		eltData.hasXhr = false
		eltData.abortable = false
		if len(eltData.queuedRequests) > 0 {
			q := eltData.queuedRequests[0]
			eltData.queuedRequests = eltData.queuedRequests[1:]
			q()
		}
	}

	var promptResponse js.Value = undefined
	if promptQuestion, ok := getClosestAttributeValue(elt, "hx-prompt"); ok {
		promptResponse = global.Call("prompt", promptQuestion)
		pd := newObject()
		pd.Set("prompt", promptResponse)
		pd.Set("target", target)
		if promptResponse.Type() == js.TypeNull || !triggerEvent(elt, "htmx:prompt", pd) {
			resolve()
			endRequestLock()
			return promise
		}
	}

	if confirmQuestion != "" && !confirmed {
		if !global.Call("confirm", confirmQuestion).Bool() {
			resolve()
			endRequestLock()
			return promise
		}
	}

	headers := getHeaders(elt, target, promptResponse)
	if verb != "get" && !usesFormData(elt) {
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if etc.headers.Truthy() {
		headers = mergeObjects(headers, etc.headers)
	}
	errors, rawFormData, _ := getInputValues(elt, verb)
	if etc.values.Truthy() {
		overrideFormData(rawFormData, formDataFromObject(etc.values))
	}
	expressionVars := formDataFromObject(getExpressionVars(elt, event))
	allFormData := overrideFormData(rawFormData, expressionVars)
	filteredFormData := filterValues(allFormData, elt)

	if cfgBool("getCacheBusterParam") && verb == "get" {
		cb := getRawAttribute(target, "id")
		if cb == "" {
			cb = "true"
		}
		filteredFormData.Call("set", "org.htmx.cache-buster", cb)
	}
	if path == "" {
		path = location().Get("href").String()
	}

	requestAttrValues := getValuesForElement(elt, "hx-request", false, undefined, undefined)
	eltIsBoosted := getInternalData(elt).boosted
	useUrlParams := contains(methodsThatUseUrlParams(), verb)

	requestConfig := newObject()
	requestConfig.Set("boosted", eltIsBoosted)
	requestConfig.Set("useUrlParams", useUrlParams)
	requestConfig.Set("formData", filteredFormData)
	requestConfig.Set("parameters", formDataProxy(filteredFormData))
	requestConfig.Set("unfilteredFormData", allFormData)
	requestConfig.Set("unfilteredParameters", formDataProxy(allFormData))
	requestConfig.Set("headers", headers)
	requestConfig.Set("elt", elt)
	requestConfig.Set("target", target)
	requestConfig.Set("verb", verb)
	requestConfig.Set("errors", errorsToJS(errors))
	requestConfig.Set("withCredentials", credOrCfg(etc, requestAttrValues))
	requestConfig.Set("timeout", timeoutOrCfg(etc, requestAttrValues))
	requestConfig.Set("path", path)
	requestConfig.Set("triggeringEvent", event)

	if !triggerEvent(elt, "htmx:configRequest", requestConfig) {
		resolve()
		endRequestLock()
		return promise
	}

	path = requestConfig.Get("path").String()
	verb = requestConfig.Get("verb").String()
	headers = requestConfig.Get("headers")
	filteredFormData = formDataFromObject(requestConfig.Get("parameters"))
	useUrlParams = requestConfig.Get("useUrlParams").Bool()
	errorsJS := requestConfig.Get("errors")

	if errorsJS.Truthy() && errorsJS.Get("length").Int() > 0 {
		triggerEvent(elt, "htmx:validation:halted", requestConfig)
		resolve()
		endRequestLock()
		return promise
	}

	// Run the Go-native request transformers. They see the FINAL headers/params
	// (after htmx:configRequest) and run before the URL is built, before headers
	// are applied to the xhr, and before the body is encoded — so a transformer
	// that sets a header or a signature parameter reaches the wire. headers and
	// filteredFormData are mutated in place; verb/path/useUrlParams are returned.
	verb, path, useUrlParams = applyRequestTransformers(verb, path, headers, filteredFormData, useUrlParams, elt)

	splitPath := strings.SplitN(path, "#", 2)
	pathNoAnchor := splitPath[0]
	anchor := ""
	if len(splitPath) > 1 {
		anchor = splitPath[1]
	}
	finalPath := path
	if useUrlParams {
		finalPath = pathNoAnchor
		hasValues := !filteredFormData.Call("keys").Call("next").Get("done").Bool()
		if hasValues {
			if strings.Index(finalPath, "?") < 0 {
				finalPath += "?"
			} else {
				finalPath += "&"
			}
			finalPath += urlEncode(filteredFormData)
			if anchor != "" {
				finalPath += "#" + anchor
			}
		}
	}

	if !verifyPath(elt, finalPath, requestConfig) {
		triggerErrorEvent(elt, "htmx:invalidPath", requestConfig)
		reject()
		endRequestLock()
		return promise
	}

	xhr.Call("open", strings.ToUpper(verb), finalPath, true)
	xhr.Call("overrideMimeType", "text/html")
	xhr.Set("withCredentials", requestConfig.Get("withCredentials"))
	xhr.Set("timeout", requestConfig.Get("timeout"))

	if !requestAttrValues.Truthy() || !requestAttrValues.Get("noHeaders").Truthy() {
		keys := global.Get("Object").Call("keys", headers)
		for i := 0; i < keys.Get("length").Int(); i++ {
			header := keys.Index(i).String()
			safelySetHeaderValue(xhr, header, headers.Get(header))
		}
	}

	responseInfo := newObject()
	responseInfo.Set("xhr", xhr)
	responseInfo.Set("target", target)
	responseInfo.Set("requestConfig", requestConfig)
	responseInfo.Set("boosted", eltIsBoosted)
	responseInfo.Set("select", selectStr)
	pathInfo := newObject()
	pathInfo.Set("requestPath", path)
	pathInfo.Set("finalRequestPath", finalPath)
	pathInfo.Set("responsePath", null)
	pathInfo.Set("anchor", anchor)
	responseInfo.Set("pathInfo", pathInfo)

	var indicators, disableElts []js.Value

	xhr.Set("onload", js.FuncOf(func(js.Value, []js.Value) any {
		// The whole response-handling pass is Go, not a single JS call, so the JS
		// try/catch bridge cannot wrap it: this guard's reach depends on the toolchain
		// implementing recover.
		threw := safe(func() {
			hierarchy := hierarchyForElt(elt)
			pathInfo.Set("responsePath", getPathFromResponse(xhr))
			if etc.handler.Truthy() {
				etc.handler.Invoke(elt, responseInfo)
			} else {
				handleAjaxResponse(elt, responseInfo, etc)
			}
			// keepIndicators is only set (to true) on HX-Redirect/HX-Refresh
			// responses; on a normal response it is undefined. Reading .Bool()
			// on undefined panics — mirror htmx.js's `!responseInfo.keepIndicators`
			// with a truthiness test that treats missing as false.
			if !responseInfo.Get("keepIndicators").Truthy() {
				removeRequestIndicators(indicators, disableElts)
			}
			triggerEvent(elt, "htmx:afterRequest", responseInfo)
			triggerEvent(elt, "htmx:afterOnLoad", responseInfo)
			if !bodyContains(elt) {
				var secondary js.Value = null
				for len(hierarchy) > 0 && !secondary.Truthy() {
					p := hierarchy[0]
					hierarchy = hierarchy[1:]
					if bodyContains(p) {
						secondary = p
					}
				}
				if secondary.Truthy() {
					triggerEvent(secondary, "htmx:afterRequest", responseInfo)
					triggerEvent(secondary, "htmx:afterOnLoad", responseInfo)
				}
			}
			resolve()
		})
		if threw {
			triggerErrorEvent(elt, "htmx:onLoadError", responseInfo)
		}
		endRequestLock()
		return nil
	}))
	xhr.Set("onerror", js.FuncOf(func(js.Value, []js.Value) any {
		removeRequestIndicators(indicators, disableElts)
		triggerErrorEvent(elt, "htmx:afterRequest", responseInfo)
		triggerErrorEvent(elt, "htmx:sendError", responseInfo)
		reject()
		endRequestLock()
		return nil
	}))
	xhr.Set("onabort", js.FuncOf(func(js.Value, []js.Value) any {
		removeRequestIndicators(indicators, disableElts)
		triggerErrorEvent(elt, "htmx:afterRequest", responseInfo)
		triggerErrorEvent(elt, "htmx:sendAbort", responseInfo)
		reject()
		endRequestLock()
		return nil
	}))
	xhr.Set("ontimeout", js.FuncOf(func(js.Value, []js.Value) any {
		removeRequestIndicators(indicators, disableElts)
		triggerErrorEvent(elt, "htmx:afterRequest", responseInfo)
		triggerErrorEvent(elt, "htmx:timeout", responseInfo)
		reject()
		endRequestLock()
		return nil
	}))
	if !triggerEvent(elt, "htmx:beforeRequest", responseInfo) {
		resolve()
		endRequestLock()
		return promise
	}
	indicators = addRequestIndicatorClasses(elt)
	disableElts = disableElements(elt)

	for _, eventName := range []string{"loadstart", "loadend", "progress", "abort"} {
		en := eventName
		for _, tgt := range []js.Value{xhr, xhr.Get("upload")} {
			tgt.Call("addEventListener", en, js.FuncOf(func(_ js.Value, a []js.Value) any {
				ev := a[0]
				d := newObject()
				d.Set("lengthComputable", ev.Get("lengthComputable"))
				d.Set("loaded", ev.Get("loaded"))
				d.Set("total", ev.Get("total"))
				triggerEvent(elt, "htmx:xhr:"+en, d)
				return nil
			}))
		}
	}
	triggerEvent(elt, "htmx:beforeSend", responseInfo)
	var params js.Value = null
	if !useUrlParams {
		params = encodeParamsForBody(xhr, elt, filteredFormData)
	}
	xhr.Call("send", params)
	return promise
}

func errorsToJS(errors []js.Value) js.Value {
	arr := global.Get("Array").New()
	for _, e := range errors {
		arr.Call("push", e)
	}
	return arr
}

func credOrCfg(etc *ajaxEtc, requestAttrValues js.Value) js.Value {
	if etc.hasCredentials {
		return etc.credentials
	}
	if requestAttrValues.Truthy() && requestAttrValues.Get("credentials").Truthy() {
		return requestAttrValues.Get("credentials")
	}
	return cfg("withCredentials")
}

func timeoutOrCfg(etc *ajaxEtc, requestAttrValues js.Value) js.Value {
	if etc.hasTimeout {
		return etc.timeout
	}
	if requestAttrValues.Truthy() && requestAttrValues.Get("timeout").Truthy() {
		return requestAttrValues.Get("timeout")
	}
	return cfg("timeout")
}

func methodsThatUseUrlParams() []string {
	arr := cfg("methodsThatUseUrlParams")
	var out []string
	for i := 0; i < arr.Get("length").Int(); i++ {
		out = append(out, arr.Index(i).String())
	}
	return out
}

type historyUpdate struct {
	updateType string // "push" | "replace" | ""
	path       string
}

func determineHistoryUpdates(elt js.Value, responseInfo js.Value, etc *ajaxEtc) historyUpdate {
	xhr := responseInfo.Get("xhr")
	var pathFromHeaders, typeFromHeaders string
	switch {
	case hasHeader(xhr, "HX-Push:"):
		pathFromHeaders = jstr(xhr.Call("getResponseHeader", "HX-Push"))
		typeFromHeaders = "push"
	case hasHeader(xhr, "HX-Push-Url:"):
		pathFromHeaders = jstr(xhr.Call("getResponseHeader", "HX-Push-Url"))
		typeFromHeaders = "push"
	case hasHeader(xhr, "HX-Replace-Url:"):
		pathFromHeaders = jstr(xhr.Call("getResponseHeader", "HX-Replace-Url"))
		typeFromHeaders = "replace"
	}
	if pathFromHeaders != "" {
		if pathFromHeaders == "false" {
			return historyUpdate{}
		}
		return historyUpdate{updateType: typeFromHeaders, path: pathFromHeaders}
	}

	pathInfo := responseInfo.Get("pathInfo")
	requestPath := jstr(pathInfo.Get("finalRequestPath"))
	responsePath := jstr(pathInfo.Get("responsePath"))

	pushUrl := ""
	if etc.hasPush {
		pushUrl = etc.push
	} else if v, _ := getClosestAttributeValue(elt, "hx-push-url"); v != "" {
		pushUrl = v
	}
	replaceUrl := ""
	if etc.hasReplace {
		replaceUrl = etc.replace
	} else if v, _ := getClosestAttributeValue(elt, "hx-replace-url"); v != "" {
		replaceUrl = v
	}
	if pushUrl == "false" {
		pushUrl = ""
	}
	if replaceUrl == "false" {
		replaceUrl = ""
	}
	elementIsBoosted := getInternalData(elt).boosted

	saveType := ""
	path := ""
	switch {
	case pushUrl != "":
		saveType = "push"
		path = pushUrl
	case replaceUrl != "":
		saveType = "replace"
		path = replaceUrl
	case elementIsBoosted:
		saveType = "push"
		path = responsePath
		if path == "" {
			path = requestPath
		}
	}
	if path != "" {
		if path == "true" {
			path = responsePath
			if path == "" {
				path = requestPath
			}
		}
		anchor := jstr(pathInfo.Get("anchor"))
		if anchor != "" && strings.Index(path, "#") == -1 {
			path = path + "#" + anchor
		}
		return historyUpdate{updateType: saveType, path: path}
	}
	return historyUpdate{}
}

func codeMatches(responseHandlingConfig js.Value, status int) bool {
	re := global.Get("RegExp").New(responseHandlingConfig.Get("code"))
	return re.Call("test", strconv.Itoa(status)).Bool()
}

func resolveResponseHandling(xhr js.Value) js.Value {
	rh := cfg("responseHandling")
	for i := 0; i < rh.Get("length").Int(); i++ {
		el := rh.Index(i)
		if codeMatches(el, xhr.Get("status").Int()) {
			return el
		}
	}
	out := newObject()
	out.Set("swap", false)
	return out
}

func handleTitle(title string) {
	if title != "" {
		titleElt := find(js.ValueOf("title"), "")
		if titleElt.Truthy() {
			titleElt.Set("textContent", title)
		} else {
			global.Get("document").Set("title", title)
		}
	}
}

func resolveRetarget(elt js.Value, target string) js.Value {
	if target == "this" {
		return elt
	}
	resolvedTarget := asElement(querySelectorExt(elt, target))
	if !resolvedTarget.Truthy() {
		d := newObject()
		d.Set("target", target)
		triggerErrorEvent(elt, "htmx:targetError", d)
		panic("Invalid re-target " + target)
	}
	return resolvedTarget
}

func handleAjaxResponse(elt js.Value, responseInfo js.Value, etc *ajaxEtc) {
	xhr := responseInfo.Get("xhr")
	target := responseInfo.Get("target")
	responseInfoSelect := jstr(responseInfo.Get("select"))

	if !triggerEvent(elt, "htmx:beforeOnLoad", responseInfo) {
		return
	}
	if hasHeader(xhr, "HX-Trigger:") {
		handleTriggerHeader(xhr, "HX-Trigger", elt)
	}
	if hasHeader(xhr, "HX-Location:") {
		redirectPath := jstr(xhr.Call("getResponseHeader", "HX-Location"))
		redirectSwapSpec := newObject()
		if strings.Index(redirectPath, "{") == 0 {
			redirectSwapSpec = parseJSON(redirectPath)
			redirectPath = jstr(redirectSwapSpec.Get("path"))
			redirectSwapSpec.Delete("path")
		}
		if !redirectSwapSpec.Get("push").Truthy() {
			redirectSwapSpec.Set("push", "true")
		}
		ajaxHelper("get", redirectPath, redirectSwapSpec)
		return
	}
	shouldRefresh := hasHeader(xhr, "HX-Refresh:") && jstr(xhr.Call("getResponseHeader", "HX-Refresh")) == "true"
	if hasHeader(xhr, "HX-Redirect:") {
		responseInfo.Set("keepIndicators", true)
		global.Get("location").Set("href", xhr.Call("getResponseHeader", "HX-Redirect"))
		if shouldRefresh {
			global.Get("location").Call("reload")
		}
		return
	}
	if shouldRefresh {
		responseInfo.Set("keepIndicators", true)
		global.Get("location").Call("reload")
		return
	}

	historyUpd := determineHistoryUpdates(elt, responseInfo, etc)
	responseHandling := resolveResponseHandling(xhr)
	shouldSwap := responseHandling.Get("swap").Bool()
	isError := responseHandling.Get("error").Truthy()
	ignoreTitle := cfgBool("ignoreTitle") || responseHandling.Get("ignoreTitle").Truthy()
	selectOverride := jstr(responseHandling.Get("select"))
	if responseHandling.Get("target").Truthy() {
		responseInfo.Set("target", resolveRetarget(elt, responseHandling.Get("target").String()))
		target = responseInfo.Get("target")
	}
	swapOverride := ""
	hasSwapOverride := etc.hasSwapOverride
	if hasSwapOverride {
		swapOverride = etc.swapOverride
	}
	if !hasSwapOverride && responseHandling.Get("swapOverride").Truthy() {
		swapOverride = responseHandling.Get("swapOverride").String()
		hasSwapOverride = true
	}
	if hasHeader(xhr, "HX-Retarget:") {
		responseInfo.Set("target", resolveRetarget(elt, jstr(xhr.Call("getResponseHeader", "HX-Retarget"))))
		target = responseInfo.Get("target")
	}
	if hasHeader(xhr, "HX-Reswap:") {
		swapOverride = jstr(xhr.Call("getResponseHeader", "HX-Reswap"))
		hasSwapOverride = true
	}

	serverResponse := jstr(xhr.Get("response"))
	beforeSwapDetails := newObject()
	beforeSwapDetails.Set("shouldSwap", shouldSwap)
	beforeSwapDetails.Set("serverResponse", serverResponse)
	beforeSwapDetails.Set("isError", isError)
	beforeSwapDetails.Set("ignoreTitle", ignoreTitle)
	beforeSwapDetails.Set("selectOverride", selectOverride)
	beforeSwapDetails.Set("swapOverride", swapOverride)
	mergeObjects(beforeSwapDetails, responseInfo)

	if responseHandling.Get("event").Truthy() &&
		!triggerEvent(target, responseHandling.Get("event").String(), beforeSwapDetails) {
		return
	}
	if !triggerEvent(target, "htmx:beforeSwap", beforeSwapDetails) {
		return
	}
	target = beforeSwapDetails.Get("target")
	serverResponse = jstr(beforeSwapDetails.Get("serverResponse"))
	isError = beforeSwapDetails.Get("isError").Truthy()
	ignoreTitle = beforeSwapDetails.Get("ignoreTitle").Truthy()
	selectOverride = jstr(beforeSwapDetails.Get("selectOverride"))
	swapOverride = jstr(beforeSwapDetails.Get("swapOverride"))
	hasSwapOverride = beforeSwapDetails.Get("swapOverride").Truthy()

	responseInfo.Set("target", target)
	responseInfo.Set("failed", isError)
	responseInfo.Set("successful", !isError)

	if beforeSwapDetails.Get("shouldSwap").Bool() {
		if xhr.Get("status").Int() == 286 {
			cancelPolling(elt)
		}
		withExtensions(elt, func(extension js.Value) {
			if v, ok := jsTry(extension, "transformResponse", serverResponse, xhr, elt); ok {
				serverResponse = jstr(v)
			}
		})
		if historyUpd.updateType != "" {
			saveCurrentPageToHistory()
		}
		spec := getSwapSpecification(elt, swapOverride, hasSwapOverride)
		if !spec.hasIgnoreTitle {
			spec.ignoreTitle = ignoreTitle
		}
		target.Get("classList").Call("add", cfgStr("swappingClass"))
		if responseInfoSelect != "" {
			selectOverride = responseInfoSelect
		}
		if hasHeader(xhr, "HX-Reselect:") {
			selectOverride = jstr(xhr.Call("getResponseHeader", "HX-Reselect"))
		}
		selectOOB := etc.selectOOB
		if !etc.hasSelectOOB {
			selectOOB, _ = getClosestAttributeValue(elt, "hx-select-oob")
		}
		selectAttr, _ := getClosestAttributeValue(elt, "hx-select")

		selectFinal := selectOverride
		if selectOverride == "unset" {
			selectFinal = ""
		} else if selectOverride == "" {
			selectFinal = selectAttr
		}

		opts := &swapOptions{
			selectStr:      selectFinal,
			selectOOB:      selectOOB,
			eventInfo:      responseInfo,
			anchor:         jstr(responseInfo.Get("pathInfo").Get("anchor")),
			contextElement: elt,
			title:          undefined,
			afterSwapCallback: func() {
				if hasHeader(xhr, "HX-Trigger-After-Swap:") {
					finalElt := elt
					if !bodyContains(elt) {
						finalElt = getDocument().Get("body")
					}
					handleTriggerHeader(xhr, "HX-Trigger-After-Swap", finalElt)
				}
			},
			afterSettleCallback: func() {
				if hasHeader(xhr, "HX-Trigger-After-Settle:") {
					finalElt := elt
					if !bodyContains(elt) {
						finalElt = getDocument().Get("body")
					}
					handleTriggerHeader(xhr, "HX-Trigger-After-Settle", finalElt)
				}
			},
			beforeSwapCallback: func() {
				if historyUpd.updateType != "" {
					h := newObject()
					hu := newObject()
					hu.Set("type", historyUpd.updateType)
					hu.Set("path", historyUpd.path)
					h.Set("history", hu)
					triggerEvent(getDocument().Get("body"), "htmx:beforeHistoryUpdate", mergeObjects(h, responseInfo))
					if historyUpd.updateType == "push" {
						pushUrlIntoHistory(historyUpd.path)
						pd := newObject()
						pd.Set("path", historyUpd.path)
						triggerEvent(getDocument().Get("body"), "htmx:pushedIntoHistory", pd)
					} else {
						replaceUrlInHistory(historyUpd.path)
						pd := newObject()
						pd.Set("path", historyUpd.path)
						triggerEvent(getDocument().Get("body"), "htmx:replacedInHistory", pd)
					}
				}
			},
		}
		swap(target, serverResponse, spec, opts)
	}
	if isError {
		d := newObject()
		d.Set("error", "Response Status Error Code "+strconv.Itoa(xhr.Get("status").Int())+" from "+jstr(responseInfo.Get("pathInfo").Get("requestPath")))
		triggerErrorEvent(elt, "htmx:responseError", mergeObjects(d, responseInfo))
	}
}

// ---------------------------------------------------------------------------
// Extensions API
// ---------------------------------------------------------------------------

var extensions = map[string]js.Value{}

func extensionBase() js.Value {
	base := newObject()
	base.Set("init", js.FuncOf(func(js.Value, []js.Value) any { return null }))
	base.Set("getSelectors", js.FuncOf(func(js.Value, []js.Value) any { return null }))
	base.Set("onEvent", js.FuncOf(func(js.Value, []js.Value) any { return true }))
	base.Set("transformResponse", js.FuncOf(func(_ js.Value, a []js.Value) any { return a[0] }))
	base.Set("isInlineSwap", js.FuncOf(func(js.Value, []js.Value) any { return false }))
	base.Set("handleSwap", js.FuncOf(func(js.Value, []js.Value) any { return false }))
	base.Set("encodeParameters", js.FuncOf(func(js.Value, []js.Value) any { return null }))
	return base
}

var internalAPIObject js.Value

func defineExtension(name string, extension js.Value) {
	if extension.Get("init").Truthy() {
		extension.Call("init", internalAPIObject)
	}
	extensions[name] = mergeObjects(extensionBase(), extension)
}

func removeExtension(name string) { delete(extensions, name) }

func getExtensions(elt js.Value, extensionsToReturn []js.Value, extensionsToIgnore []string) []js.Value {
	if !elt.Truthy() {
		return extensionsToReturn
	}
	extensionsForElement := getAttributeValue(elt, "hx-ext")
	if extensionsForElement != "" {
		for _, extensionName := range strings.Split(extensionsForElement, ",") {
			extensionName = strings.ReplaceAll(extensionName, " ", "")
			if strings.Index(extensionName, "ignore:") == 0 {
				extensionsToIgnore = append(extensionsToIgnore, extensionName[7:])
				continue
			}
			if !contains(extensionsToIgnore, extensionName) {
				if extension, ok := extensions[extensionName]; ok {
					if !containsValue(extensionsToReturn, extension) {
						extensionsToReturn = append(extensionsToReturn, extension)
					}
				}
			}
		}
	}
	return getExtensions(asElement(parentElt(elt)), extensionsToReturn, extensionsToIgnore)
}

func containsValue(arr []js.Value, v js.Value) bool {
	for _, e := range arr {
		if e.Equal(v) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Initialization
// ---------------------------------------------------------------------------

var isReady = false

// isStarted guards Start() so the initial document-processing pass runs at most
// once even if Start() (or Boot()) is called more than once.
var isStarted = false

func ready(fn func()) {
	if isReady || getDocument().Get("readyState").String() == "complete" {
		fn()
	} else {
		getDocument().Call("addEventListener", "DOMContentLoaded", js.FuncOf(func(js.Value, []js.Value) any {
			fn()
			return nil
		}))
	}
}

func insertIndicatorStyles() {
	if cfgBool("includeIndicatorStyles") {
		nonceAttribute := ""
		if n := cfgStr("inlineStyleNonce"); n != "" {
			nonceAttribute = ` nonce="` + n + `"`
		}
		indicator := cfgStr("indicatorClass")
		request := cfgStr("requestClass")
		getDocument().Get("head").Call("insertAdjacentHTML", "beforeend",
			"<style"+nonceAttribute+">"+
				"."+indicator+"{opacity:0;visibility: hidden} "+
				"."+request+" ."+indicator+", ."+request+"."+indicator+"{opacity:1;visibility: visible;transition: opacity 200ms ease-in}"+
				"</style>")
	}
}

func getMetaConfig() js.Value {
	element := getDocument().Call("querySelector", `meta[name="htmx-config"]`)
	if element.Truthy() {
		return parseJSON(element.Get("content").String())
	}
	return null
}

func mergeMetaConfig() {
	metaConfig := getMetaConfig()
	if metaConfig.Truthy() {
		config = mergeObjects(config, metaConfig)
		htmxObj.Set("config", config)
	}
}

func buildConfig() js.Value {
	c := newObject()
	c.Set("historyEnabled", true)
	c.Set("historyCacheSize", 10)
	c.Set("refreshOnHistoryMiss", false)
	c.Set("defaultSwapStyle", "innerHTML")
	c.Set("defaultSwapDelay", 0)
	c.Set("defaultSettleDelay", 20)
	c.Set("includeIndicatorStyles", true)
	c.Set("indicatorClass", "htmx-indicator")
	c.Set("requestClass", "htmx-request")
	c.Set("addedClass", "htmx-added")
	c.Set("settlingClass", "htmx-settling")
	c.Set("swappingClass", "htmx-swapping")
	c.Set("allowEval", true)
	c.Set("allowScriptTags", true)
	c.Set("inlineScriptNonce", "")
	c.Set("inlineStyleNonce", "")
	c.Set("attributesToSettle", strArray("class", "style", "width", "height"))
	c.Set("withCredentials", false)
	c.Set("timeout", 0)
	c.Set("wsReconnectDelay", "full-jitter")
	c.Set("wsBinaryType", "blob")
	c.Set("disableSelector", "[hx-disable], [data-hx-disable]")
	c.Set("scrollBehavior", "instant")
	c.Set("defaultFocusScroll", false)
	c.Set("getCacheBusterParam", false)
	c.Set("globalViewTransitions", false)
	c.Set("methodsThatUseUrlParams", strArray("get", "delete"))
	c.Set("selfRequestsOnly", true)
	c.Set("ignoreTitle", false)
	c.Set("scrollIntoViewOnBoost", true)
	c.Set("triggerSpecsCache", null)
	c.Set("disableInheritance", false)
	rh := global.Get("Array").New()
	rh.Call("push", respHandling("204", false, false))
	rh.Call("push", respHandling("[23]..", true, false))
	rh.Call("push", respHandling("[45]..", false, true))
	c.Set("responseHandling", rh)
	c.Set("allowNestedOobSwaps", true)
	c.Set("historyRestoreAsHxRequest", true)
	c.Set("reportValidityOfForms", false)
	return c
}

func respHandling(code string, swapB, errorB bool) js.Value {
	o := newObject()
	o.Set("code", code)
	o.Set("swap", swapB)
	if errorB {
		o.Set("error", true)
	}
	return o
}

func strArray(items ...string) js.Value {
	arr := global.Get("Array").New()
	for _, it := range items {
		arr.Call("push", it)
	}
	return arr
}

// parseSwapSpec builds a swapSpecT from a public htmx.swap() spec object.
func parseSwapSpec(o js.Value) *swapSpecT {
	spec := &swapSpecT{swapStyle: cfgStr("defaultSwapStyle"), swapDelay: cfgFloat("defaultSwapDelay"), settleDelay: cfgFloat("defaultSettleDelay")}
	if !o.Truthy() {
		return spec
	}
	if v := o.Get("swapStyle"); v.Truthy() {
		spec.swapStyle = v.String()
	}
	if v := o.Get("swapDelay"); v.Type() == js.TypeNumber {
		spec.swapDelay = v.Float()
	}
	if v := o.Get("settleDelay"); v.Type() == js.TypeNumber {
		spec.settleDelay = v.Float()
	}
	if v := o.Get("ignoreTitle"); v.Type() == js.TypeBoolean {
		spec.ignoreTitle = v.Bool()
		spec.hasIgnoreTitle = true
	}
	if v := o.Get("scroll"); v.Truthy() {
		spec.scroll = jstr(v)
	}
	if v := o.Get("show"); v.Truthy() {
		spec.show = jstr(v)
	}
	return spec
}

func parseSwapOptions(o js.Value) *swapOptions {
	opts := &swapOptions{eventInfo: undefined, title: undefined, contextElement: undefined}
	if !o.Truthy() {
		return opts
	}
	if v := o.Get("select"); v.Truthy() {
		opts.selectStr = v.String()
	}
	if v := o.Get("selectOOB"); v.Truthy() {
		opts.selectOOB = v.String()
	}
	if v := o.Get("eventInfo"); v.Truthy() {
		opts.eventInfo = v
	}
	if v := o.Get("anchor"); v.Truthy() {
		opts.anchor = v.String()
	}
	if v := o.Get("contextElement"); v.Truthy() {
		opts.contextElement = v
	}
	if v := o.Get("title"); v.Truthy() {
		opts.title = v
	}
	return opts
}

// fn wraps a Go function as a persistent JS function value.
func fn(f func(this js.Value, args []js.Value) any) js.Value {
	return js.FuncOf(f).Value
}

func argOr(args []js.Value, i int) js.Value {
	if i < len(args) {
		return args[i]
	}
	return undefined
}

func installAPI() {
	htmxObj = newObject()
	config = buildConfig()

	htmxObj.Set("onLoad", fn(func(_ js.Value, a []js.Value) any { return onLoadHelper(a[0]) }))
	htmxObj.Set("process", fn(func(_ js.Value, a []js.Value) any { processNode(a[0]); return nil }))
	htmxObj.Set("registerTriggerAlias", fn(func(_ js.Value, a []js.Value) any {
		RegisterTriggerAlias(jstr(argOr(a, 0)), jstr(argOr(a, 1)), jstr(argOr(a, 2)))
		return nil
	}))
	htmxObj.Set("on", fn(func(_ js.Value, a []js.Value) any {
		return addEventListenerImpl(argOr(a, 0), argOr(a, 1), argOr(a, 2), argOr(a, 3))
	}))
	htmxObj.Set("off", fn(func(_ js.Value, a []js.Value) any {
		return removeEventListenerImpl(argOr(a, 0), argOr(a, 1), argOr(a, 2))
	}))
	htmxObj.Set("trigger", fn(func(_ js.Value, a []js.Value) any {
		return triggerEvent(resolveTarget(a[0], undefined), a[1].String(), argOr(a, 2))
	}))
	htmxObj.Set("ajax", fn(func(_ js.Value, a []js.Value) any {
		return ajaxHelper(a[0].String(), a[1].String(), argOr(a, 2))
	}))
	htmxObj.Set("find", fn(func(_ js.Value, a []js.Value) any {
		if len(a) >= 2 {
			return find(a[0], a[1].String())
		}
		return find(a[0], "")
	}))
	htmxObj.Set("findAll", fn(func(_ js.Value, a []js.Value) any {
		if len(a) >= 2 {
			return findAll(a[0], a[1].String())
		}
		return findAll(a[0], "")
	}))
	htmxObj.Set("closest", fn(func(_ js.Value, a []js.Value) any { return closest(a[0], a[1].String()) }))
	htmxObj.Set("values", fn(func(_ js.Value, a []js.Value) any {
		verb := "post"
		if len(a) > 1 && a[1].Truthy() {
			verb = a[1].String()
		}
		_, _, values := getInputValues(resolveTarget(a[0], undefined), verb)
		return values
	}))
	htmxObj.Set("remove", fn(func(_ js.Value, a []js.Value) any {
		delay := 0.0
		if len(a) > 1 && a[1].Type() == js.TypeNumber {
			delay = a[1].Float()
		}
		removeElement(a[0], delay)
		return nil
	}))
	htmxObj.Set("addClass", fn(func(_ js.Value, a []js.Value) any {
		delay := 0.0
		if len(a) > 2 && a[2].Type() == js.TypeNumber {
			delay = a[2].Float()
		}
		addClassToElement(a[0], a[1].String(), delay)
		return nil
	}))
	htmxObj.Set("removeClass", fn(func(_ js.Value, a []js.Value) any {
		delay := 0.0
		if len(a) > 2 && a[2].Type() == js.TypeNumber {
			delay = a[2].Float()
		}
		removeClassFromElement(a[0], a[1].String(), delay)
		return nil
	}))
	htmxObj.Set("toggleClass", fn(func(_ js.Value, a []js.Value) any { toggleClassOnElement(a[0], a[1].String()); return nil }))
	htmxObj.Set("takeClass", fn(func(_ js.Value, a []js.Value) any { takeClassForElement(a[0], a[1].String()); return nil }))
	htmxObj.Set("swap", fn(func(_ js.Value, a []js.Value) any {
		swap(resolveTarget(a[0], undefined), a[1].String(), parseSwapSpec(argOr(a, 2)), parseSwapOptions(argOr(a, 3)))
		return nil
	}))
	htmxObj.Set("defineExtension", fn(func(_ js.Value, a []js.Value) any { defineExtension(a[0].String(), a[1]); return nil }))
	htmxObj.Set("removeExtension", fn(func(_ js.Value, a []js.Value) any { removeExtension(a[0].String()); return nil }))
	htmxObj.Set("logAll", fn(func(js.Value, []js.Value) any { logAll(); return nil }))
	htmxObj.Set("logNone", fn(func(js.Value, []js.Value) any { logNone(); return nil }))
	htmxObj.Set("logger", null)
	htmxObj.Set("config", config)
	htmxObj.Set("parseInterval", fn(func(_ js.Value, a []js.Value) any {
		if len(a) == 0 || a[0].Type() != js.TypeString {
			return undefined
		}
		if iv, ok := parseInterval(a[0].String()); ok {
			return iv
		}
		return undefined
	}))
	htmxObj.Set("location", global.Get("location"))
	htmxObj.Set("_", fn(func(_ js.Value, a []js.Value) any { return internalEval(a[0].String()) }))
	htmxObj.Set("version", Version)

	// internalAPI exposed to extensions (subset; the surface most extensions use).
	internalAPIObject = newObject()
	internalAPIObject.Set("getInternalData", fn(func(_ js.Value, a []js.Value) any {
		// internal data is Go-side; expose the JS expando bag for compatibility.
		d := a[0].Get("__htmxGoDataBag")
		if !d.Truthy() {
			d = newObject()
			a[0].Set("__htmxGoDataBag", d)
		}
		return d
	}))
	internalAPIObject.Set("triggerEvent", fn(func(_ js.Value, a []js.Value) any {
		return triggerEvent(a[0], a[1].String(), argOr(a, 2))
	}))
	internalAPIObject.Set("bodyContains", fn(func(_ js.Value, a []js.Value) any { return bodyContains(a[0]) }))
	internalAPIObject.Set("makeFragment", fn(func(_ js.Value, a []js.Value) any { return makeFragment(a[0].String()) }))
	internalAPIObject.Set("mergeObjects", fn(func(_ js.Value, a []js.Value) any { return mergeObjects(a[0], a[1]) }))

	global.Set("htmx", htmxObj)
}

// Install installs window.htmx and the framework-level APIs — the request
// transformer registry (RegisterTransformer), the extension API
// (defineExtension), htmx.config, and the dummyElt / hx-on xpath / history
// bootstrap — WITHOUT processing the document. It is idempotent.
//
// Splitting install from the initial DOM pass lets callers register extensions
// and request transformers after window.htmx exists but before anything is
// processed, which is what fixes the extension-registration ordering problem:
//
//	htmx.Install()      // window.htmx exists; RegisterTransformer/defineExtension usable
//	preload.Register()  // define extensions BEFORE the first processNode(body)
//	htmx.Start()        // now process the document (extensions already defined)
func Install() {
	if htmxObj.Truthy() {
		return
	}
	installAPI()

	dummyElt = getDocument().Call("createElement", "output")
	hxOnXPath = global.Get("XPathEvaluator").New().Call("createExpression",
		`.//*[@*[ starts-with(name(), "hx-on:") or starts-with(name(), "data-hx-on:") or`+
			` starts-with(name(), "hx-on-") or starts-with(name(), "data-hx-on-") ]]`)

	// A WASM host installs asynchronously and can arrive after DOMContentLoaded, which the listener below would then never catch; readyState is "loading" only while the parser runs, so anything else proves the document is parsed and ready() must not park work on an event that cannot fire again.
	if getDocument().Get("readyState").String() != "loading" {
		isReady = true
	}

	getDocument().Call("addEventListener", "DOMContentLoaded", js.FuncOf(func(js.Value, []js.Value) any {
		isReady = true
		return nil
	}))

	setCurrentPathForHistory(location().Get("pathname").String() + location().Get("search").String())
}

// Start runs the initial document-processing pass (processNode(body)), wires the
// history/abort/popstate listeners, and fires htmx:load. Call it once, after
// Install() and after any extensions / request transformers are registered. It
// is idempotent.
//
// Ordering note (for the eventual TinyGo core boot): Start installs its
// listeners synchronously and only THEN processes the DOM. Keep this
// "install synchronously, then process" ordering so the DOM pass never
// synchronously re-enters an unwinding component turn.
func Start() {
	if isStarted {
		return
	}
	isStarted = true
	ready(func() {
		mergeMetaConfig()
		insertIndicatorStyles()
		body := getDocument().Get("body")
		processNode(body)
		restoredElts := getDocument().Call("querySelectorAll",
			`[hx-trigger='restored'],[data-hx-trigger='restored']`)
		body.Call("addEventListener", "htmx:abort", js.FuncOf(func(_ js.Value, a []js.Value) any {
			evt := a[0]
			target := evt.Get("detail").Get("elt")
			if !target.Truthy() {
				target = evt.Get("target")
			}
			internalData := getInternalData(target)
			if internalData.hasXhr {
				internalData.xhr.Call("abort")
			}
			return nil
		}))
		// Scope-tied request cancellation. When a subtree is torn down during an
		// htmx swap, cleanUpElement fires "htmx:beforeCleanupElement" on every
		// element being removed, while it is still attached — so the event
		// bubbles here. If such an element has an in-flight request, abort it by
		// re-firing htmx's own "htmx:abort": the body listener above then calls
		// xhr.abort(), so this reuses htmx's cancellation path and keeps
		// hx-sync/request state consistent (no raw double-abort). This stops a
		// response from swapping into a now-detached scope and prevents an
		// unmounted component's request from lingering. The readyState guard
		// skips a normal self-replacing swap, whose request has already
		// completed (readyState 4, endRequestLock not yet run at cleanup time),
		// so no spurious abort event fires on successful completion.
		body.Call("addEventListener", "htmx:beforeCleanupElement", js.FuncOf(func(_ js.Value, a []js.Value) any {
			evt := a[0]
			target := evt.Get("detail").Get("elt")
			if !target.Truthy() {
				target = evt.Get("target")
			}
			if !target.Truthy() {
				return nil
			}
			d := getInternalData(target)
			if d.hasXhr && d.xhr.Truthy() && d.xhr.Get("readyState").Int() != 4 {
				triggerEvent(target, "htmx:abort", undefined)
			}
			return nil
		}))
		// hx-publish (host-app bridge): after any swap, if the swapped element (or
		// an ancestor) carries hx-publish="<name>", dispatch <name> — rewritten
		// through the alias table (e.g. "topic:cart" → "gothic:topic:cart") — as a
		// bubbling CustomEvent on document, so hx-trigger subscribers (including the
		// aliased topic: triggers) fire. A single delegated listener on body catches
		// every afterSwap by bubbling; it is a no-op unless an element opts in, so it
		// never touches the swap/trigger core paths. The dispatch carries no payload
		// (a notify-style publish): host value-subscribers that read a byte buffer
		// safely no-op on a bufferless event, while htmx subscribers re-fetch.
		body.Call("addEventListener", "htmx:afterSwap", js.FuncOf(func(_ js.Value, a []js.Value) any {
			target := asElement(a[0].Get("target"))
			if !target.Truthy() {
				return nil
			}
			pubElt := target.Call("closest", "[hx-publish],[data-hx-publish]")
			if !pubElt.Truthy() {
				return nil
			}
			raw := getAttributeValue(pubElt, "hx-publish")
			if raw == "" {
				return nil
			}
			name, _, _ := resolveTriggerAlias(raw)
			init := newObject()
			init.Set("bubbles", true)
			detail := newObject()
			detail.Set("source", pubElt)
			init.Set("detail", detail)
			evt := global.Get("CustomEvent").New(name, init)
			getDocument().Call("dispatchEvent", evt)
			return nil
		}))
		originalPopstate := global.Get("onpopstate")
		global.Set("onpopstate", js.FuncOf(func(_ js.Value, a []js.Value) any {
			event := a[0]
			if event.Get("state").Truthy() && event.Get("state").Get("htmx").Truthy() {
				restoreHistory("")
				for _, elt := range jsArrayToSlice(restoredElts) {
					d := newObject()
					d.Set("document", getDocument())
					triggerEvent(elt, "htmx:restored", d)
				}
			} else if originalPopstate.Truthy() && originalPopstate.Type() == js.TypeFunction {
				originalPopstate.Invoke(event)
			}
			return nil
		}))
		getWindow().Call("setTimeout", js.FuncOf(func(js.Value, []js.Value) any {
			triggerEvent(body, "htmx:load", newObject())
			return nil
		}), 0)
	})
}

// Boot installs window.htmx and initialises the current document. Idempotent.
// It is exactly Install() followed by Start(), so the standalone cmd/htmx-wasm
// entrypoint is unchanged. Callers that must register extensions or request
// transformers before the initial DOM pass call Install() and Start() directly
// with their registration in between.
func Boot() {
	Install()
	Start()
}
