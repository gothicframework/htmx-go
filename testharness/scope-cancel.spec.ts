import { test, expect } from "@playwright/test";

// Scope-tied request cancellation.
//
// When a subtree is torn down during an htmx swap, htmx's cleanUpElement fires
// "htmx:beforeCleanupElement" on every element being removed (while it is still
// attached, so the event bubbles to body). The port installs a delegated body
// listener that, for a torn-down element with an in-flight request, re-fires
// htmx's own "htmx:abort" — reusing htmx's cancellation path so xhr.abort()
// runs and hx-sync/request state stays consistent.
//
// This is exactly what Gothic needs: when a `[data-gothic-scope]` component is
// swapped out, any XHR issued from inside it is aborted, so a response can never
// swap into a now-detached scope and an unmounted component's request cannot
// linger. The mechanism rides htmx's own swap teardown and needs no
// Gothic-specific coupling (the `data-gothic-scope` marker below is only there
// to mirror a real Gothic component; the abort keys off the in-flight XHR, not
// the marker).

async function bootReady(page: import("@playwright/test").Page) {
  // Record the relevant htmx lifecycle events on document, keyed by the id of
  // the element they targeted, so assertions can be precise.
  await page.addInitScript(() => {
    const w = window as any;
    w.__evt = { abort: [], sendAbort: [], afterSwap: [], beforeReq: [] };
    const idOf = (e: Event) => {
      const d = (e as CustomEvent).detail;
      const elt = (d && d.elt) || (e.target as Element);
      return (elt && (elt as Element).id) || "";
    };
    document.addEventListener("htmx:beforeRequest", (e) => w.__evt.beforeReq.push(idOf(e)));
    document.addEventListener("htmx:abort", (e) => w.__evt.abort.push(idOf(e)));
    document.addEventListener("htmx:sendAbort", (e) => w.__evt.sendAbort.push(idOf(e)));
    document.addEventListener("htmx:afterSwap", (e) => w.__evt.afterSwap.push(idOf(e)));
  });

  await page.goto("/");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 15_000,
  });
  await expect(page.locator("body")).not.toHaveAttribute("data-wasm-error", /.+/);
}

// Inject a scoped component: a wrapper (the "scope") containing a button that
// issues a slow request into a sink INSIDE the same scope, plus a remover button
// OUTSIDE the scope that replaces the scope's contents (tearing it down).
const FIXTURE = `
  <div id="scope-remover-host"></div>
  <div id="scope-host">
    <div id="the-scope" data-gothic-scope="test-scope">
      <button id="scoped-slow"
              hx-get="/slow?ms=4000" hx-target="#scoped-slow-out" hx-swap="innerHTML">slow</button>
      <div id="scoped-slow-out">scope-sink-original</div>
    </div>
  </div>`;

test("aborts an in-flight request when its scope subtree is swapped out", async ({ page }) => {
  await bootReady(page);

  // Mount the fixture and let htmx wire it.
  await page.evaluate((html) => {
    const host = document.createElement("div");
    host.id = "cancel-fixture";
    host.innerHTML = html;
    document.body.appendChild(host);
    (window as any).htmx.process(host);
  }, FIXTURE);

  // Fire the slow request from inside the scope.
  await page.click("#scoped-slow");

  // Confirm the request is genuinely in flight (beforeRequest fired for it, and
  // htmx is holding an XHR handle on the issuing element).
  await expect
    .poll(() => page.evaluate(() => (window as any).__evt.beforeReq.includes("scoped-slow")))
    .toBe(true);

  // Tear the scope down mid-flight by replacing #scope-host's contents via an
  // htmx swap. This is what removes #the-scope (and the issuing #scoped-slow),
  // triggering htmx's cleanUpElement → htmx:beforeCleanupElement → abort.
  await page.evaluate(() => {
    (window as any).htmx.ajax("GET", "/fragment", { target: "#scope-host", swap: "innerHTML" });
  });

  // The issuing element must be gone…
  await expect(page.locator("#scoped-slow")).toHaveCount(0);

  // …and its in-flight request must have been aborted via htmx's own path:
  // htmx:abort re-fired on the element, and the XHR's onabort handler ran
  // (htmx:sendAbort). Both keyed to the issuing element id.
  await expect
    .poll(() => page.evaluate(() => (window as any).__evt.abort.includes("scoped-slow")))
    .toBe(true);
  await expect
    .poll(() => page.evaluate(() => (window as any).__evt.sendAbort.includes("scoped-slow")))
    .toBe(true);

  // Prove no response ever swaps into the dead scope: wait well past the point
  // the server would have replied (4000ms) and assert the slow payload never
  // landed anywhere, and no afterSwap fired for the aborted request's sink.
  await page.waitForTimeout(4500);
  const swappedIntoDeadScope = await page.evaluate(
    () => (window as any).__evt.afterSwap.includes("scoped-slow-out"),
  );
  expect(swappedIntoDeadScope).toBe(false);
  const slowPayloadPresent = await page.evaluate(
    () => document.body.textContent?.includes("slow-ok") ?? false,
  );
  expect(slowPayloadPresent).toBe(false);
});

test("does NOT abort a request that completes normally (no teardown)", async ({ page }) => {
  // Regression guard: the cleanup-abort seam must not touch requests whose
  // element is never torn down — a normal slow request still completes and swaps.
  await bootReady(page);

  await page.evaluate((html) => {
    const host = document.createElement("div");
    host.id = "complete-fixture";
    host.innerHTML = html;
    document.body.appendChild(host);
    (window as any).htmx.process(host);
  }, FIXTURE.replace("/slow?ms=4000", "/slow?ms=600"));

  await page.click("#scoped-slow");

  // The request completes and swaps its payload into the (still-attached) sink.
  await expect(page.locator("#scoped-slow-out")).toContainText("slow-ok", { timeout: 5_000 });

  // No abort of any kind fired for it.
  const aborted = await page.evaluate(
    () =>
      (window as any).__evt.abort.includes("scoped-slow") ||
      (window as any).__evt.sendAbort.includes("scoped-slow"),
  );
  expect(aborted).toBe(false);
});
