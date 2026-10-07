//go:build ignore

// Command server is the host-side (NOT js/wasm) static + fragment server for the
// htmx-go WASM validation harness. It is guarded with the `ignore` build tag so a
// wildcard `GOOS=js GOARCH=wasm go build ./...` never tries to compile it.
//
// Run it from the module root:
//
//	cd htmx-go && go run testharness/server/main.go
//	# or pick a port:  PORT=9001 go run testharness/server/main.go
//
// It serves the harness static files (index.html, wasm_exec.js, htmx.wasm) and a
// set of HTML-fragment endpoints the Playwright spec asserts against:
//
//	/fragment        a plain swap payload with a stable testid
//	/echo            reflects method + urlencoded body + the X-Gothic-Test header
//	                 (which a htmx:config:request listener injects) so the round trip
//	                 of the header/parameter-mutation contract is observable
//	/oob             primary payload + an hx-swap-oob="true" element for #oob-target
//	/tick            monotonically increasing counter, for `every 1s` polling
//	/slow            a deliberately delayed payload, for abort tests
//	/script-fragment HTML containing an inline <script> (sets window.__scriptFragmentRan)
//	                 plus a NESTED hx-get button, to prove swapped scripts run and
//	                 htmx re-wires the swapped subtree
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// staticDir resolves to the testharness/ directory (the parent of this file's
// server/ dir) via runtime.Caller, so the server works no matter the cwd.
func staticDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "testharness"
	}
	return filepath.Dir(filepath.Dir(thisFile))
}

var tickCounter atomic.Int64

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8899"
	}
	root := staticDir()

	mux := http.NewServeMux()

	// ── static files with explicit content types ──
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" {
			p = "/index.html"
		}
		clean := filepath.Clean(filepath.Join(root, filepath.FromSlash(p)))
		// contain within root
		if !strings.HasPrefix(clean, root) {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(clean, ".wasm"):
			w.Header().Set("Content-Type", "application/wasm")
		case strings.HasSuffix(clean, ".js"):
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(clean, ".html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		// no-cache so a rebuilt wasm is always picked up
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, clean)
	})

	// ── fragment endpoints ──
	mux.HandleFunc("/fragment", func(w http.ResponseWriter, r *http.Request) {
		writeHTML(w, `<span data-testid="fragment-payload">fragment-ok</span>`)
	})

	mux.HandleFunc("/tick", func(w http.ResponseWriter, r *http.Request) {
		n := tickCounter.Add(1)
		writeHTML(w, fmt.Sprintf(`<span data-testid="tick-payload">tick-%d</span>`, n))
	})

	mux.HandleFunc("/oob", func(w http.ResponseWriter, r *http.Request) {
		// Primary content swaps into hx-target; the oob element is routed to
		// #oob-target by id regardless of hx-target.
		writeHTML(w, `<span data-testid="oob-payload">primary-ok</span>`+
			`<div id="oob-target" hx-swap-oob="true" data-testid="oob-target">oob-swapped</div>`)
	})

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		delay := 1500 * time.Millisecond
		if d := r.URL.Query().Get("ms"); d != "" {
			if ms, err := strconv.Atoi(d); err == nil {
				delay = time.Duration(ms) * time.Millisecond
			}
		}
		select {
		case <-time.After(delay):
			writeHTML(w, `<span data-testid="slow-payload">slow-ok</span>`)
		case <-r.Context().Done():
			// client aborted (htmx:abort → xhr.abort) — nothing to write
			return
		}
	})

	mux.HandleFunc("/script-fragment", func(w http.ResponseWriter, r *http.Request) {
		// Inline <script> must execute after the swap; the nested hx-get button
		// must become live only if htmx re-processes the swapped subtree.
		writeHTML(w, `<span data-testid="script-payload">script-ok</span>`+
			`<button data-testid="script-nested" hx-get="/fragment" hx-target="#script-nested-out" hx-swap="innerHTML">nested</button>`+
			`<div id="script-nested-out" data-testid="script-nested-out">nested empty</div>`+
			`<script>window.__scriptFragmentRan = true;</script>`)
	})

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		// Status-code driven fixtures: ?code=<n> returns that status with a
		// swap payload, for hx-status:<pattern> override assertions.
		code := 200
		if c := r.URL.Query().Get("code"); c != "" {
			if n, err := strconv.Atoi(c); err == nil {
				code = n
			}
		}
		w.WriteHeader(code)
		writeHTML(w, `<span data-testid="status-payload">status-ok</span>`)
	})

	mux.HandleFunc("/wrapper", func(w http.ResponseWriter, r *http.Request) {
		// A wrapped payload for outerSync / boost-HCON-target assertions.
		writeHTML(w, `<div id="wrapper-payload-id" class="adopted-class" data-testid="wrapper-payload">`+
			`<span data-testid="wrapped-text">wrapped-ok</span></div>`)
	})

	mux.HandleFunc("/morph-inner", func(w http.ResponseWriter, r *http.Request) {
		// Same-id span as the morph-inner fixture: innerMorph must patch it in
		// place rather than destroy the (id-carrying) node.
		writeHTML(w, `<span data-testid="morph-span" id="morph-span">morph-inner-new</span>`)
	})

	mux.HandleFunc("/morph-outer", func(w http.ResponseWriter, r *http.Request) {
		// Same-id div as the outerMorph fixture: the id-carrying target must be
		// morphed (patched, node kept) rather than replaced wholesale.
		writeHTML(w, `<div id="morph-target" class="fresh-class" data-testid="morph-target">`+
			`<span data-testid="morph-outer-text">morph-outer-new</span></div>`)
	})

	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		// Re-parse the raw body ourselves so we reflect exactly what was sent.
		values := parseURLEncoded(string(raw))
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		fmt.Fprintf(&b, "method=%s\n", r.Method)
		fmt.Fprintf(&b, "header:X-Gothic-Test=%s\n", r.Header.Get("X-Gothic-Test"))
		fmt.Fprintf(&b, "header:X-Transform-Order=%s\n", r.Header.Get("X-Transform-Order"))
		fmt.Fprintf(&b, "header:x-amz-content-sha256=%s\n", r.Header.Get("x-amz-content-sha256"))
		fmt.Fprintf(&b, "header:HX-Request=%s\n", r.Header.Get("HX-Request"))
		fmt.Fprintf(&b, "body=%s\n", string(raw))
		for _, k := range keys {
			fmt.Fprintf(&b, "param:%s=%s\n", k, strings.Join(values[k], ","))
		}
		writeHTML(w, `<span data-testid="echo-payload">`+htmlEscape(b.String())+`</span>`)
	})

	// ── sigv4 signing harness page ──
	// Renders the AWS SigV4 signing harness. The provider marker
	// (<meta name="gothic-provider" content="AWS">) is emitted ONLY when the query
	// carries ?provider=AWS, so the same htmx-sigv4.wasm can be exercised BOTH with
	// the gate on (header present) and off (marker absent → sigv4.Register installs
	// nothing → header absent). The marker is rendered server-side into the parsed
	// <head> before the wasm boots, so the signer reads it race-free at Register().
	mux.HandleFunc("/sigv4", func(w http.ResponseWriter, r *http.Request) {
		marker := ""
		if r.URL.Query().Get("provider") == "AWS" {
			marker = `<meta name="gothic-provider" content="AWS">`
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, sigv4Page(marker))
	})

	addr := ":" + port
	log.Printf("htmx-go validation harness serving %s at http://localhost%s/", root, addr)
	log.Printf("ready signal: body[data-wasm-ready='1']  (or the window 'wasm-ready' event)")
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// sigv4Page renders the AWS SigV4 signing harness. `marker` is either the
// provider <meta> tag (AWS gate on) or "" (gate off). Three controls exercise the
// signer's decision tree, each echoing the reflected x-amz-content-sha256 header
// into its own sink:
//   - POST urlencoded form  -> header == sha256hex(the exact urlencoded body)
//   - GET (useUrlParams)    -> header == EmptyBodyHash
//   - multipart (hx-encoding=multipart/form-data) -> header == UNSIGNED-PAYLOAD
func sigv4Page(marker string) string {
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>htmx-go sigv4 signing harness</title>
  ` + marker + `
</head>
<body>
  <h1>htmx-go sigv4 signing harness</h1>

  <!-- POST urlencoded: body = alpha=one&beta=two ; header must be sha256hex(body). -->
  <form id="post-form" data-testid="post-form"
        hx-post="/echo" hx-target="#post-out" hx-swap="innerHTML">
    <input type="text" name="alpha" value="one">
    <input type="text" name="beta" value="two">
    <button type="submit" data-testid="post-submit">POST /echo</button>
  </form>
  <pre id="post-out" data-testid="post-out"></pre>

  <!-- GET: useUrlParams, body empty -> EmptyBodyHash (independent of any params). -->
  <button data-testid="get-btn" hx-get="/echo"
          hx-target="#get-out" hx-swap="innerHTML">GET /echo</button>
  <pre id="get-out" data-testid="get-out"></pre>

  <!-- multipart: hx-encoding forces a random-boundary body -> UNSIGNED-PAYLOAD. -->
  <form id="multipart-form" data-testid="multipart-form"
        hx-post="/echo" hx-encoding="multipart/form-data"
        hx-target="#multipart-out" hx-swap="innerHTML">
    <input type="text" name="delta" value="four">
    <button type="submit" data-testid="multipart-submit">POST /echo (multipart)</button>
  </form>
  <pre id="multipart-out" data-testid="multipart-out"></pre>

  <!-- EMPTY multipart: no named controls, so the FormData is empty and EncodedBody
       is "" — but the browser still writes the closing boundary bytes, so the body
       is NOT empty. Must sign UNSIGNED-PAYLOAD (multipart checked before empty). -->
  <form id="empty-multipart-form" data-testid="empty-multipart-form"
        hx-post="/echo" hx-encoding="multipart/form-data"
        hx-target="#empty-multipart-out" hx-swap="innerHTML">
    <button type="submit" data-testid="empty-multipart-submit">POST /echo (empty multipart)</button>
  </form>
  <pre id="empty-multipart-out" data-testid="empty-multipart-out"></pre>

  <script src="/wasm_exec.js"></script>
  <script>
    (async function bootWasm() {
      try {
        const go = new Go();
        const result = await WebAssembly.instantiateStreaming(
          fetch("/htmx-sigv4.wasm"), go.importObject);
        go.run(result.instance);
        if (!window.htmx) throw new Error("go.run() returned but window.htmx is undefined");
        document.body.dataset.wasmReady = "1";
        window.dispatchEvent(new CustomEvent("wasm-ready"));
      } catch (err) {
        document.body.dataset.wasmError = String(err && err.message || err);
        console.error("wasm boot failed:", err);
      }
    })();
  </script>
</body>
</html>`
}

func writeHTML(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, s)
}

// parseURLEncoded is a tiny application/x-www-form-urlencoded splitter that keeps
// the raw ordering intact per key (net/url would work too but this makes the
// reflected output trivially predictable for assertions).
func parseURLEncoded(body string) map[string][]string {
	out := map[string][]string{}
	if body == "" {
		return out
	}
	for _, pair := range strings.Split(body, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k = urlDecode(k)
		v = urlDecode(v)
		out[k] = append(out[k], v)
	}
	return out
}

func urlDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, ok1 := fromHex(s[i+1]); ok1 {
				if lo, ok2 := fromHex(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
