import { test, expect } from "@playwright/test";

// Smoke test: proves the harness boots the compiled htmx.wasm cleanly. It does
// NOT assert htmx behaviour — that is the full QA spec's job. It confirms only:
//   1. the page reaches the deterministic ready state,
//   2. window.htmx is installed and reports the upstream version,
//   3. ZERO console errors AND zero page errors occurred *during instantiation*
//      (i.e. up to and including the moment window.htmx is installed / ready is
//      signalled — which happens synchronously at the end of htmx.Boot(), before
//      any XHR onload can fire),
//   4. the only post-ready console errors, if any, are the known htmx:onLoadError
//      runtime defect (see the harness README / report) — a new/unexpected error
//      class fails the smoke test.
//
// Why the "at ready" distinction matters: the boot-time `load` and `every`
// triggers dispatch requests during processNode, but their onload callbacks run
// on later ticks. Those callbacks currently hit a port defect (js.Value.Bool on
// an undefined `keepIndicators`, htmx.go:3853) and emit htmx:onLoadError AFTER
// the swap has already landed. That is a Go/WASM port bug for a later phase, not
// an instantiation failure — so we pin instantiation cleanliness precisely.
test("harness boots htmx.wasm with a clean instantiation", async ({ page }) => {
  const pageErrors: string[] = [];
  const consoleErrors: string[] = [];
  page.on("pageerror", (err) => pageErrors.push(String(err)));
  page.on("console", (msg) => {
    if (msg.type() === "error") consoleErrors.push(msg.text());
  });

  // Instrument BEFORE any page script runs: count console.error calls and
  // snapshot that count at the instant the wasm-ready event fires.
  await page.addInitScript(() => {
    const w = window as any;
    w.__errCount = 0;
    w.__errCountAtReady = -1;
    const orig = console.error.bind(console);
    console.error = (...args: any[]) => {
      w.__errCount++;
      orig(...args);
    };
    window.addEventListener("wasm-ready", () => {
      w.__errCountAtReady = w.__errCount;
    });
  });

  await page.goto("/");

  // Deterministic ready signal — never networkidle (the `every 1s` trigger keeps
  // the network perpetually busy).
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 15_000,
  });
  await expect(page.locator("body")).not.toHaveAttribute("data-wasm-error", /.+/);

  // window.htmx installed and reporting the upstream version.
  const version = await page.evaluate(() => (window as any).htmx?.version);
  expect(version).toBe("2.0.9");
  const htmxType = await page.evaluate(() => typeof (window as any).htmx);
  expect(htmxType).toBe("object");

  // Zero console errors at the instant of readiness (instantiation was clean).
  const errCountAtReady = await page.evaluate(() => (window as any).__errCountAtReady);
  expect(errCountAtReady, "console.error count at wasm-ready").toBe(0);

  // No WASM-level page errors at all (a panic during Boot would surface here).
  expect(pageErrors, `pageerrors:\n${pageErrors.join("\n")}`).toEqual([]);

  // Give the boot-time load/every triggers a moment to complete, then confirm the
  // only console errors that appeared are the documented htmx:onLoadError defect —
  // any other error class is unexpected and fails the smoke test.
  await page.waitForTimeout(1200);
  const unexpected = consoleErrors.filter((e) => !/htmx:onLoadError/.test(e));
  expect(unexpected, `unexpected console errors:\n${unexpected.join("\n")}`).toEqual([]);
});
