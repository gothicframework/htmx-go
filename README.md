# htmx-go

A **Go/WASM 1:1 port of [htmx](https://htmx.org)**, maintained by the Gothic
Framework org. It reimplements htmx's client runtime in Go (compiled to
`GOOS=js GOARCH=wasm`), so a Go program can embed htmx directly and install a real
`window.htmx` from inside the same WASM binary. Gothic's **core** module embeds it,
giving Gothic apps a full htmx client without loading a separate JS file.

This tree mirrors **htmx v2.0.9**.

## Versioning — tracks upstream htmx

The module version follows htmx's version, not Gothic's:

| htmx | module path | example tag |
|------|-------------|-------------|
| 2.x  | `github.com/gothicframework/htmx-go/v2` | `v2.0.9` |
| 4.x  | `github.com/gothicframework/htmx-go/v4` | `v4.0.0` |

- The Go **major-version suffix** (`/v2`, `/v4`) mirrors htmx's major version.
- Semver tags mirror upstream htmx releases (this is `v2.0.9`).
- `htmx.version` reported on `window` is the upstream string (`2.0.9`), so JS
  tooling and extensions see the real version.

When htmx ships an update, we ship the matching Go mirror.

## Usage

It's an importable library, not a binary. A consumer (e.g. Gothic's core WASM
runtime) boots it from a `js/wasm` main:

```go
//go:build js && wasm

package main

import htmx "github.com/gothicframework/htmx-go/v2"

func main() {
	htmx.Boot()  // installs window.htmx and processes document.body
	select {}    // keep the Go runtime + listeners alive
}
```

Build it standalone with the bundled command:

```sh
GOOS=js GOARCH=wasm go build -o core.wasm ./cmd/htmx-wasm
```

`Boot()` is idempotent, installs `window.htmx`, and wires the current document. After
that it behaves like the htmx script.

### `Install()` / `Start()` — the boot split

`Boot()` is exactly `Install(); Start()`. When you need to register transformers or
extensions **before** the first request, call the two halves yourself:

- **`Install()`** — installs `window.htmx` plus the extension and transformer APIs
  (`defineExtension`, `RegisterTransformer`, `RegisterTriggerAlias`). It does **not**
  touch the DOM yet, so anything you register here is in place before any element is
  processed.
- **`Start()`** — processes `document.body` and begins the normal htmx lifecycle
  (triggers, polling, history, first `load` requests).

```go
htmx.Install()      // install window.htmx + transformer/extension API (no DOM processing)
sigv4.Register()    // in-path AWS signer (registers only on AWS — see ext/sigv4)
preload.Register()  // htmx preload extension
htmx.Start()        // now wire the document; the first request already sees both
```

This ordering matters: an in-path signer or a preload extension registered *after*
`Start()` would miss requests fired by the initial DOM pass.

## The `RequestTransformer` seam

htmx-go exposes an **in-path request seam** that upstream htmx has no equivalent for.
A `RequestTransformer` is a Go function run **inside** the request pipeline — after
`htmx:configRequest`, immediately before `xhr.send` — so it can rewrite the outgoing
request with no boot race and no dependence on a JS event listener firing first.

```go
type RequestTransformer func(req *TransformableRequest)

func RegisterTransformer(t RequestTransformer) // transformers run in registration order
```

`TransformableRequest` gives a transformer full, ordered access to the request just
before it goes out:

- `Verb` / `Path` / `UseURLParams` — settable fields (move params between URL and body,
  rewrite the method or path).
- `GetHeader(name)` / `SetHeader(name, value)` / `Headers()` — read and overwrite headers
  (this is how a signer adds an `Authorization` / `x-amz-*` header).
- `GetParam` / `SetParam` / `AppendParam` / `Params()` — read and mutate request params.
- `EncodedBody()` — the exact urlencoded body htmx would send (so a body-hash signer
  hashes precisely what ships).
- `Element()` — the triggering element, as read-only request context.

Example — stamp a correlation header on every request:

```go
htmx.RegisterTransformer(func(req *htmx.TransformableRequest) {
	if req.GetHeader("X-Request-Id") == "" {
		req.SetHeader("X-Request-Id", newRequestID())
	}
})
```

Because it runs as a line in the request path rather than as a `htmx:configRequest`
event listener, a transformer is guaranteed to see **every** request — including the
first one fired by the initial DOM pass — with no ordering caveats. `ext/sigv4` builds
on this to sign requests safely.

## What this is

`htmx.go` is a **direct, function-by-function translation** of `htmx.js` (v2.0.9)
— not a reimplementation. Function names mirror the originals (`processNode`,
`getTriggerSpecs`, `issueAjaxRequest`, `swapWithStyle`, `handleAjaxResponse`,
`getInputValues`, …) so the two files diff side by side, and the same runtime
machinery is preserved:

- The **internal-data model** (htmx's per-element/-event private state), keyed
  Go-side so it can hold timers, listener records, and closures.
- **`XMLHttpRequest`** request cycle (not `fetch`) — including `hx-sync` queue
  strategies (`drop`/`abort`/`replace`/`queue`), `hx-confirm`/`hx-prompt`,
  indicators, `hx-disabled-elt`, and the full `htmx:configRequest` →
  `beforeSend` → `onload`/`onerror`/`onabort`/`ontimeout` lifecycle.
- The **trigger tokenizer** (`tokenizeString` / `consumeCSSSelector` /
  `parseAndCacheTrigger`) with every modifier: `delay`, `throttle`, `once`,
  `changed`, `from`, `target`, `consume`, `queue`, `root`, `threshold`, plus
  `every` polling, `load`, `revealed`, `intersect`.
- The complete **swap + settle pipeline**: all swap styles, `hx-swap-oob`,
  `select`/`select-oob`, attribute settling (`cloneAttributes`), preserved
  elements, focus/selection restoration, scroll/`show`, title handling, and the
  swap/settle **delays**.
- **History** support: session-storage cache, `saveToHistoryCache` /
  `restoreHistory` / `loadHistoryFromServer`, `HX-Push-Url` / `HX-Replace-Url`,
  and `popstate` restore.
- Attribute **inheritance / disinheritance** (`hx-inherit` / `hx-disinherit`),
  `hx-params`, `hx-vals` / `hx-vars`, `hx-headers`, `hx-encoding`, `hx-request`.
- The **extension API** (`defineExtension` / `getExtensions` / `withExtensions`)
  and the `hx-on:*` wildcard handlers.
- The full **public API** on `window.htmx` (`process`, `ajax`, `trigger`, `on`,
  `off`, `onLoad`, `find`, `findAll`, `closest`, `values`, `remove`, `addClass`,
  `removeClass`, `toggleClass`, `takeClass`, `swap`, `defineExtension`,
  `removeExtension`, `logAll`/`logNone`, `parseInterval`, `config`, `version`),
  plus the htmx-go additions `registerTriggerAlias` and the in-path transformer API.

Where htmx uses `new Function` / `eval` (event filters like `[event.detail]`,
`hx-vals`/`hx-vars` `js:` expressions, `hx-on:` handlers), the port **delegates
to the browser's JS engine** via `syscall/js` — the faithful behaviour, since it
runs in the browser.

## Deliberate deviations

A few branches lean on the JS engine or differ slightly from a pure line-for-line
copy; all are behaviour-preserving:

- **Internal data is keyed by a Go-side map** (via a `__htmxGoId` expando) rather
  than a raw JS expando, because the data holds Go closures/timers.
- **View transitions** (`document.startViewTransition`) are applied inline; the
  Promise-wrapped settle path is folded into the settle step.
- **eval-dependent features** run through `Function`/`eval` (see above) rather
  than a Go expression evaluator.

## Testing

Two layers of browser tests cover the port against a live DOM:

- **`testharness/`** — a Playwright harness that loads `htmx.wasm` + `wasm_exec.js`
  in a real page and exercises request/swap/history/trigger behaviour, the
  `RequestTransformer` seam, `ext/sigv4`, scope-tied cancellation, and the Topic
  bridge (≈60 browser tests across the spec files).
- **The Gothic e2e Playwright gate** runs the whole framework — with htmx supplied
  by `core.wasm` — end to end (~193 of 198 passing). The one non-passing case is an
  unrelated TinyGo 0.41.1 runtime issue (a `SetFinalizer` leak, fixed upstream by a
  pending TinyGo release), not an htmx-go or Gothic bug.

## Extensions (`ext/`)

Ported htmx extensions/plugins live under `ext/` as independent subpackages, each
with a `Register()` entrypoint and the reference `.js` (where one exists) kept
beside it for diffing. Two mechanisms:

- **Public-API extensions** depend only on `window.htmx` (`defineExtension`) and
  link à la carte — e.g. `ext/preload`.
- **In-path transformer plugins** register a Go `RequestTransformer` into the
  htmx-go request seam (run inside the request path, before `send`) — e.g.
  `ext/sigv4`. Not a `configRequest` listener, so no boot-race.

| Extension | Upstream | Mechanism | Marginal size |
|---|---|---|---|
| `ext/preload` | `htmx-ext-preload` v2.0.1 | extension (`defineExtension`) | +3.5 KB brotli |
| `ext/sigv4` | (Gothic's AWS SigV4 signer) | in-path `RequestTransformer` | small |

Because htmx-go installs a real `window.htmx` (with `defineExtension`), **users can
still load any third-party htmx extension as a normal JS `<script>`** — the Go
`ext/` ports are just the ones Gothic ships built-in.

**`ext/sigv4`** signs requests with `x-amz-content-sha256` as an in-path
`RequestTransformer` (after `htmx:configRequest`, before `xhr.send`) — it hashes the
exact body htmx would send. It is gated by a **runtime** signal, not a build flag:
the same committed `core.wasm` serves every app, so `Register()` reads the
server-rendered `<meta name="gothic-provider" content="AWS">` marker and installs the
transformer **only** when it reads `AWS`. Off AWS the marker is absent and `Register()`
installs nothing. Because the signer is part of the request path rather than a listener
that must attach in time, emitting an unsigned request on AWS is impossible by control
flow.

**`ext/preload`** is the htmx preload extension port. In Gothic core it ships as an
**unconditional built-in** (no config flag) — the runtime always registers it — while
in htmx-go itself it remains an à-la-carte extension you `Register()`.

## Integration with Gothic

htmx-go exposes seams that Gothic core wires into deeper runtime behaviour:

- **Scope-tied request cancellation.** When a Gothic stateful component unmounts, any
  in-flight htmx XHR it owns is aborted. This reuses htmx's own `htmx:abort` via a
  single delegated `htmx:beforeCleanupElement` listener — no bespoke cancellation
  machinery, just htmx's existing abort path tied to the component scope.
- **Topic ⇄ htmx bridge** (`RegisterTriggerAlias`). A trigger or publish name that
  begins with a registered prefix is rewritten to a host bus event:
  - `hx-trigger="topic:<name>"` fires when a Gothic Topic publishes `<name>` — the
    alias rewrites `topic:<name>` to the host event (e.g. `gothic:topic:<name>`) and,
    absent an explicit `from:`, binds the listener where the bus dispatches.
  - `hx-publish="topic:<name>"` publishes to that Topic on `htmx:afterSwap` — a single
    delegated `body` listener dispatches the rewritten event after any swap.
  - `RegisterTriggerAlias(prefix, eventPrefix, defaultFrom)` registers the mapping and
    is also exposed as `htmx.registerTriggerAlias` on `window`. Call it before
    `Start()` so the initial DOM pass sees the alias.

## Tracking upstream — `ext/versions.json` + the watch workflow

The versions this repo's ports track are pinned in **one place**: `ext/versions.json`.
It records, per port, the upstream repo and the pinned version:

| Port | Upstream | Pinned | Where the code says so |
|---|---|---|---|
| `htmx` | `bigskysoftware/htmx` (release) | `2.0.9` | `htmx.go` `const Version` |
| `preload` | `bigskysoftware/htmx-extensions` → `src/preload/package.json` | `2.0.1` | `ext/preload/preload.go` header |
| `sigv4` | (Gothic-original — no upstream package) | `n/a` | `ext/sigv4/` |

> **Bumping a port means updating BOTH the code marker AND the matching `pinned`
> value in `ext/versions.json`.** They must stay in sync — the manifest is the
> source of truth the watcher reads.

A scheduled GitHub Action (`.github/workflows/upstream-watch.yml`, daily +
manual `workflow_dispatch`) runs the detector in `.github/upstream-watch/` (its
**own** Go module, kept out of the wasm build). It compares each pin to upstream:

- **htmx** — latest GitHub release tag (`releases/latest`).
- **preload** — the `version` in `src/preload/package.json` on the default branch
  (htmx-extensions has no per-extension release tags).
- **sigv4** — informational only; never flagged (a human reviews the AWS SigV4 spec).

On drift it opens a single `upstream-update`-labelled issue for that version
(idempotent — no duplicate for a version already tracked). It is **notify-only**:
it never opens a PR and never edits a pin. Run the detector locally with
`cd .github/upstream-watch && GOWORK=off go run . --manifest ../../ext/versions.json`
(host program — normal Go, not js/wasm).

## Layout

```
htmx-go/
├── go.mod                 # module .../htmx-go/v2, tracks htmx 2.x
├── htmx.go                # the Go/WASM port (~4.9k lines, package htmx)
├── htmx.js                # upstream source this mirrors (v2.0.9), for diffing
├── transformer.go         # the in-path RequestTransformer seam
├── cmd/htmx-wasm/         # standalone build entrypoint
├── testharness/           # Playwright browser-validation harness (+ built .wasm)
├── ext/
│   ├── versions.json      # single source of truth for upstream pins
│   ├── preload/           # htmx-ext-preload v2.0.1 port (+ preload.js reference)
│   └── sigv4/             # AWS SigV4 sha256 signer, in-path transformer
└── .github/
    ├── workflows/upstream-watch.yml  # daily notify-only upstream-drift check
    └── upstream-watch/    # standalone Go module: the drift detector (+ tests)
```

## Size

Including htmx in `core.wasm` adds roughly **+230 KB brotli** to the core binary.
Compared with loading htmx as a standalone `htmx.min.js` (~14 KB brotli), the net
additional client transfer is about **+214 KB brotli** — carried inside `core.wasm`,
which is deferred and non-render-blocking, so it stays off the critical rendering path.
