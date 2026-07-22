#!/usr/bin/env bash
# Regenerate the WASM runtime glue + the compiled htmx.wasm for the validation
# harness. Deterministic: safe to run in CI or by the QA agent before a test run.
#
#   testharness/build.sh
#
# Produces:
#   testharness/wasm_exec.js    copied verbatim from the active Go toolchain
#   testharness/htmx.wasm       built from ./cmd/htmx-preload-wasm (htmx + preload)
#   testharness/htmx-sigv4.wasm built from ./cmd/htmx-sigv4-wasm (htmx + sigv4 + preload)
set -euo pipefail

# Resolve paths relative to this script so cwd does not matter.
HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(cd "${HARNESS_DIR}/.." && pwd)"

# The wasm_exec.js that matches the toolchain building the .wasm (copy, never symlink).
GOROOT="$(go env GOROOT)"
WASM_EXEC="${GOROOT}/lib/wasm/wasm_exec.js"
if [[ ! -f "${WASM_EXEC}" ]]; then
  # older layouts kept it under misc/wasm
  WASM_EXEC="${GOROOT}/misc/wasm/wasm_exec.js"
fi
cp "${WASM_EXEC}" "${HARNESS_DIR}/wasm_exec.js"
echo "copied wasm_exec.js from ${WASM_EXEC}"

# Build the harness wasm: boots htmx AND registers the preload extension.
( cd "${MODULE_DIR}" && GOWORK=off GOOS=js GOARCH=wasm go build -o testharness/htmx.wasm ./cmd/htmx-preload-wasm )
echo "built testharness/htmx.wasm ($(du -h "${HARNESS_DIR}/htmx.wasm" | cut -f1))"

# Build the sigv4 signing harness wasm: boots htmx, registers the sigv4 signer
# (runtime-gated on the server-rendered provider marker) and the preload extension.
( cd "${MODULE_DIR}" && GOWORK=off GOOS=js GOARCH=wasm go build -o testharness/htmx-sigv4.wasm ./cmd/htmx-sigv4-wasm )
echo "built testharness/htmx-sigv4.wasm ($(du -h "${HARNESS_DIR}/htmx-sigv4.wasm" | cut -f1))"
