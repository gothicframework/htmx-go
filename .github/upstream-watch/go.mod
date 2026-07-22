// Standalone host-side tool that the upstream-watch GitHub Action runs to detect
// when an upstream project this repo ports has moved ahead of its pin. It is a
// normal host program (NOT js/wasm) and is deliberately its OWN module so the
// main htmx-go wasm build (GOOS=js GOARCH=wasm go build ./...) never touches it.
// It has no third-party dependencies — stdlib only.
module github.com/gothicframework/htmx-go/upstream-watch

go 1.26
