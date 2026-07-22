#!/usr/bin/env bash
# Build the WASM harness, then start the host static + fragment server.
#
#   testharness/run.sh            # serves on :8899
#   PORT=9001 testharness/run.sh  # serves on :9001
#
# Prints the URL to open. Wait deterministically on the ready signal set by
# index.html once the wasm instantiates:
#   body[data-wasm-ready="1"]   (or the window "wasm-ready" event)
# NEVER wait on networkidle — the `every 1s` polling trigger keeps the network busy.
set -euo pipefail

HARNESS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$(cd "${HARNESS_DIR}/.." && pwd)"
PORT="${PORT:-8899}"

"${HARNESS_DIR}/build.sh"

echo "open: http://localhost:${PORT}/"
cd "${MODULE_DIR}"
PORT="${PORT}" exec go run testharness/server/main.go
