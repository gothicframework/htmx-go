import { test, expect, Page } from "@playwright/test";

// ─────────────────────────────────────────────────────────────────────────────
// htmx-go WASM port — behavioural assertion spec.
//
// This spec asserts the compiled Go/WASM port of htmx v4.0.0 against the live DOM
// fixtures in index.html + the fragment endpoints in server/main.go. It is a
// VALIDATION spec: it does NOT patch htmx.go. Where the port diverges from
// upstream htmx, the divergence is captured as a real, failing assertion —
// never a skip.
//
// Design rules:
//   • Every behaviour is asserted on its own OBSERVABLE outcome — the DOM
//     mutation or the network round-trip — not merely on a lifecycle event.
//   • Readiness is deterministic: body[data-wasm-ready="1"]. NEVER networkidle —
//     the `every 1s` poll keeps the network perpetually busy.
//   • Locator/`expect` auto-retry is used instead of fixed sleeps wherever a
//     positive DOM signal exists. Fixed waits are only used to assert a NEGATIVE
//     (something must NOT happen), where there is nothing to poll on.
// ─────────────────────────────────────────────────────────────────────────────

const FRAGMENT_TEXT = "fragment-ok";

/** Navigate and block until the WASM runtime has installed window.htmx. */
async function gotoReady(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 15_000,
  });
  await expect(page.locator("body")).not.toHaveAttribute("data-wasm-error", /.+/);
}

/** Per-test capture of console.error text + uncaught page errors. */
function captureErrors(page: Page): { console: string[]; page: string[] } {
  const bag = { console: [] as string[], page: [] as string[] };
  page.on("console", (m) => {
    if (m.type() === "error") bag.console.push(m.text());
  });
  page.on("pageerror", (e) => bag.page.push(String(e)));
  return bag;
}

// ── install + identity ───────────────────────────────────────────────────────

test.describe("install + identity", () => {
  test("window.htmx is installed as an object", async ({ page }) => {
    await gotoReady(page);
    const type = await page.evaluate(() => typeof (window as any).htmx);
    expect(type).toBe("object");
  });

  test("htmx.version === 4.0.0", async ({ page }) => {
    await gotoReady(page);
    const version = await page.evaluate(() => (window as any).htmx?.version);
    expect(version).toBe("4.0.0");
  });
});

// ── basic request / response round-trip ───────────────────────────────────────

test.describe("basic requests", () => {
  test("hx-get swaps the returned fragment into the target (innerHTML)", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("get-basic").click();
    await expect(page.getByTestId("get-out")).toContainText(FRAGMENT_TEXT);
  });

  test("hx-post form sends the urlencoded body /echo reflects back", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("echo-submit").click();
    const out = page.getByTestId("echo-out");
    await expect(out).toContainText("method=POST");
    // The exact urlencoded body the form serialized and sent.
    await expect(out).toContainText("body=alpha=one&beta=two");
    await expect(out).toContainText("param:alpha=one");
    await expect(out).toContainText("param:beta=two");
  });
});

// ── configRequest signer contract: headers AND parameters must be mutable ──────

test.describe("htmx:config:request signer contract", () => {
  test("configRequest event fires on a request", async ({ page }) => {
    await gotoReady(page);
    await page.evaluate(() => {
      const w = window as any;
      w.__configReqCount = 0;
      document.addEventListener("htmx:config:request", () => w.__configReqCount++);
    });
    await page.getByTestId("get-basic").click();
    await expect
      .poll(() => page.evaluate(() => (window as any).__configReqCount))
      .toBeGreaterThan(0);
  });

  test("a header mutated in a configRequest listener reaches the request", async ({
    page,
  }) => {
    await gotoReady(page);
    // index.html installs an always-on listener that sets header
    // X-Gothic-Test=signed. /echo reflects the received header back, proving the
    // header round-tripped from the listener into the actual XHR.
    await page.getByTestId("echo-submit").click();
    await expect(page.getByTestId("echo-out")).toContainText(
      "header:X-Gothic-Test=signed",
    );
  });

  // Signer contract: assigning a STRING to detail.parameters[key] in a
  // htmx:config:request listener must append the value to the sent body. The
  // formData `set` proxy trap now type-checks the value before probing
  // `.forEach`, so a plain string appends (matching upstream htmx.js) instead of
  // panicking the WASM runtime. A signer that adds a signature PARAMETER (not
  // just a header) works.
  test("a STRING parameter mutated in a configRequest listener reaches the body", async ({
    page,
  }) => {
    const errs = captureErrors(page);
    await gotoReady(page);
    await page.evaluate(() => {
      document.addEventListener("htmx:config:request", (evt: any) => {
        // Signer contract: add a signature PARAMETER (a plain string value).
        evt.detail.parameters["signature"] = "sig-abc123";
      });
    });
    await page.getByTestId("echo-submit").click();
    // /echo reflects the injected param back.
    await expect(page.getByTestId("echo-out")).toContainText(
      "param:signature=sig-abc123",
    );
    // No uncaught page error (the string-set path must not panic the runtime).
    expect(errs.page, "no pageerror from string param set").toHaveLength(0);
  });
});

// ── Go-native RequestTransformer seam ─────────────────────────────────────────
//
// The harness wasm (cmd/htmx-preload-wasm) registers three RequestTransformers
// via htmx.RegisterTransformer BETWEEN htmx.Install() and htmx.Start():
//   • two append to the X-Transform-Order header ("1" then "2"),
//   • one sets a signature parameter (xform-sig=sig-from-transformer).
// They run inside issueAjaxRequest after htmx:config:request resolves and before
// xhr.send, so their mutations reach the wire. /echo reflects both back. This is
// the seam the AWS SigV4 signer will hook into (Go-native, not the JS detail).
test.describe("RequestTransformer seam", () => {
  test("a transformer-set header reaches the request, and transformers run in registration order", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("echo-submit").click();
    // Two transformers each append to X-Transform-Order; "12" proves both ran
    // AND that they ran in registration order (t1 before t2).
    await expect(page.getByTestId("echo-out")).toContainText(
      "header:X-Transform-Order=12",
    );
  });

  test("a transformer-set signature parameter reaches the body", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("echo-submit").click();
    // The sigv4-style param contract: a transformer's parameter reaches the body.
    await expect(page.getByTestId("echo-out")).toContainText(
      "param:xform-sig=sig-from-transformer",
    );
  });
});

// ── hx-swap modes ─────────────────────────────────────────────────────────────

test.describe("hx-swap modes", () => {
  test("innerHTML replaces target content", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("swap-innerHTML").click();
    await expect(page.getByTestId("swap-inner-out")).toContainText(FRAGMENT_TEXT);
    await expect(page.getByTestId("swap-inner-out")).not.toContainText(
      "original-inner",
    );
  });

  test("outerHTML replaces the target element itself", async ({ page }) => {
    await gotoReady(page);
    // Scope to the swap-modes section — a boot-time `load` trigger elsewhere on
    // the page also injects a data-testid="fragment-payload" node.
    const section = page
      .getByTestId("swap-outerHTML")
      .locator("xpath=ancestor::section");
    await page.getByTestId("swap-outerHTML").click();
    // The #swap-outer element is gone; the fragment took its place.
    await expect(page.locator("#swap-outer")).toHaveCount(0);
    await expect(section.getByTestId("fragment-payload")).toHaveCount(1);
  });

  test("beforeend appends after existing children", async ({ page }) => {
    await gotoReady(page);
    const target = page.getByTestId("swap-beforeend-out");
    await page.getByTestId("swap-beforeend").click();
    await expect(target).toContainText(FRAGMENT_TEXT);
    await expect(target).toContainText("seed"); // pre-existing child preserved
    // Appended => last child is the fragment.
    const lastTestid = await target.evaluate(
      (el) => (el.lastElementChild as HTMLElement)?.dataset.testid,
    );
    expect(lastTestid).toBe("fragment-payload");
  });

  test("afterbegin prepends before existing children", async ({ page }) => {
    await gotoReady(page);
    const target = page.getByTestId("swap-afterbegin-out");
    await page.getByTestId("swap-afterbegin").click();
    await expect(target).toContainText(FRAGMENT_TEXT);
    await expect(target).toContainText("seed");
    const firstTestid = await target.evaluate(
      (el) => (el.firstElementChild as HTMLElement)?.dataset.testid,
    );
    expect(firstTestid).toBe("fragment-payload");
  });

  test("delete removes the target from the DOM", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("swap-delete").click();
    await expect(page.locator("#swap-delete")).toHaveCount(0);
  });

  test("none leaves the target content untouched", async ({ page }) => {
    await gotoReady(page);
    const target = page.getByTestId("swap-none-out");
    await page.getByTestId("swap-none").click();
    // No positive swap signal exists for `none`; give the request time to land,
    // then assert the DOM was NOT mutated.
    await page.waitForTimeout(600);
    await expect(target).toHaveText("unchanged-by-none");
  });
});

// ── out-of-band swap ──────────────────────────────────────────────────────────

test.describe("hx-swap-oob", () => {
  test("oob element is routed to its id target, primary swaps normally", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("oob-trigger").click();
    // Primary payload lands in the hx-target.
    await expect(page.getByTestId("oob-primary")).toContainText("primary-ok");
    // OOB element (hx-swap-oob="true" id="oob-target") routed by id, replacing
    // the standing #oob-target regardless of hx-target.
    await expect(page.locator("#oob-target")).toContainText("oob-swapped");
  });
});

// ── hx-target variants ────────────────────────────────────────────────────────

test.describe("hx-target variants", () => {
  test("this — swaps into the requesting element", async ({ page }) => {
    await gotoReady(page);
    const el = page.getByTestId("target-this");
    await el.click();
    await expect(el).toContainText(FRAGMENT_TEXT);
  });

  test("closest — swaps into the nearest matching ancestor", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("target-closest").click();
    await expect(page.locator(".closest-box")).toContainText(FRAGMENT_TEXT);
  });

  test("find — swaps into the first matching descendant", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("target-find").click();
    await expect(page.getByTestId("find-sink")).toContainText(FRAGMENT_TEXT);
  });

  test("next — swaps into the next matching sibling", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("target-next").click();
    await expect(page.getByTestId("next-sink")).toContainText(FRAGMENT_TEXT);
  });

  test("previous — swaps into the previous matching sibling", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("target-previous").click();
    await expect(page.getByTestId("prev-sink")).toContainText(FRAGMENT_TEXT);
  });
});

// ── hx-trigger variants ───────────────────────────────────────────────────────

test.describe("hx-trigger variants", () => {
  test("delay — fires after the debounce window", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("trig-delay").click();
    await expect(page.getByTestId("trig-delay-out")).toContainText(FRAGMENT_TEXT);
  });

  test("throttle — the leading click fires immediately", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("trig-throttle").click();
    await expect(page.getByTestId("trig-throttle-out")).toContainText(
      FRAGMENT_TEXT,
    );
  });

  test("once — fires exactly once across repeated events", async ({ page }) => {
    await gotoReady(page);
    const btn = page.getByTestId("trig-once");
    const out = page.getByTestId("trig-once-out");
    await btn.click();
    // Wait for the first (and only permitted) request to land.
    await expect(out.getByTestId("fragment-payload")).toHaveCount(1);
    // A second click must be ignored by `once`.
    await btn.click({ force: true });
    await page.waitForTimeout(500);
    await expect(out.getByTestId("fragment-payload")).toHaveCount(1);
  });

  test("changed — fires on value change, stays silent otherwise", async ({
    page,
  }) => {
    await gotoReady(page);
    const input = page.getByTestId("trig-changed");
    const out = page.getByTestId("trig-changed-out");
    // Positive: change the value → request fires.
    await input.focus();
    await input.press("x");
    await expect(out).toContainText(FRAGMENT_TEXT);
    // Negative: a keyup that does NOT change the value must not fire.
    await out.evaluate((el) => (el.textContent = "reset"));
    await input.press("ArrowLeft"); // keyup, value unchanged
    await page.waitForTimeout(400);
    await expect(out).toHaveText("reset");
  });

  test("from: — a request fires from an event on another element", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("trig-from-source").click();
    await expect(page.getByTestId("trig-from")).toContainText(FRAGMENT_TEXT);
  });

  test("load — fires automatically as the element is processed", async ({
    page,
  }) => {
    await gotoReady(page);
    // The load trigger dispatches during the boot-time processNode pass; poll
    // for the DOM outcome.
    await expect(page.getByTestId("trig-load")).toContainText(FRAGMENT_TEXT);
  });

  test("every — polls the endpoint and keeps polling", async ({ page }) => {
    await gotoReady(page);
    const el = page.getByTestId("trig-every");
    // First tick lands.
    await expect(el).toContainText("tick-", { timeout: 4000 });
    const first = (await el.textContent())?.trim();
    // Subsequent ticks keep arriving (value advances) — proves polling did not
    // stop after the first response.
    await expect
      .poll(async () => (await el.textContent())?.trim(), { timeout: 5000 })
      .not.toBe(first);
  });

  test("revealed — fires when scrolled into view", async ({ page }) => {
    await gotoReady(page);
    const el = page.getByTestId("trig-revealed");
    await el.scrollIntoViewIfNeeded();
    await expect(el).toContainText(FRAGMENT_TEXT);
  });
});

// ── swapped <script> execution + htmx.process re-wiring ───────────────────────

test.describe("swapped script + re-wiring", () => {
  test("an inline <script> in a swapped fragment executes", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("script-trigger").click();
    await expect(page.getByTestId("script-out")).toContainText("script-ok");
    // The script set a global flag on execution.
    await expect
      .poll(() => page.evaluate(() => (window as any).__scriptFragmentRan))
      .toBe(true);
  });

  test("htmx.process re-wires nodes inserted by a swap", async ({ page }) => {
    await gotoReady(page);
    await page.getByTestId("script-trigger").click();
    // The swapped fragment carries a NESTED hx-get button. If htmx re-processed
    // the inserted subtree, clicking it fires a live request.
    const nested = page.getByTestId("script-nested");
    await expect(nested).toBeVisible();
    await nested.click();
    await expect(page.getByTestId("script-nested-out")).toContainText(
      FRAGMENT_TEXT,
    );
  });
});

// ── history (hx-push-url) ─────────────────────────────────────────────────────

test.describe("history", () => {
  test("hx-push-url updates location and back-button restores it", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.getByTestId("push-link").click();
    await expect(page.getByTestId("push-out")).toContainText(FRAGMENT_TEXT);
    // hx-push-url="true" pushes the request URL (/fragment).
    await expect
      .poll(() => page.evaluate(() => location.pathname))
      .toBe("/fragment");
    // Back button restores the prior URL.
    await page.goBack();
    await expect
      .poll(() => page.evaluate(() => location.pathname))
      .toBe("/");
  });
});

// ── preload extension ─────────────────────────────────────────────────────────

// Preload extension on STATIC markup. The harness wasm registers the preload
// extension between htmx.Install() and htmx.Start() (cmd/htmx-preload-wasm), so
// the extension is defined BEFORE the initial processNode(body) pass. Static
// markup under hx-ext="preload" therefore initialises: the anchor gets
// preloadState, preload:init fires, and hover warms the target. (The Install /
// Start split is what makes this ordering possible — extensions register in the
// gap between installing window.htmx and processing the document.)
test.describe("preload extension", () => {
  test("preload:init fires and preloadState initialises to PAUSE", async ({
    page,
  }) => {
    // Capture preload:init before boot — htmx.trigger dispatches it during the
    // boot-time processNode, bubbling up to document.
    await page.addInitScript(() => {
      (window as any).__preloadInit = 0;
      document.addEventListener("preload:init", () => {
        (window as any).__preloadInit++;
      });
    });
    await gotoReady(page);
    // After init the anchor should sit in the PAUSE state, ready to warm.
    await expect
      .poll(() => page.evaluate(() => (window as any).__preloadInit), {
        timeout: 2000,
      })
      .toBeGreaterThan(0);
    const state = await page.evaluate(
      () => (document.getElementById("preload-link") as any)?.preloadState,
    );
    expect(state).toBe("PAUSE");
  });

  test("hovering a [preload] anchor warms its target (PAUSE→…→DONE + XHR)", async ({
    page,
  }) => {
    await gotoReady(page);
    // Count /fragment requests so we can prove the preload issued a fresh one.
    let fragmentReqs = 0;
    page.on("request", (r) => {
      if (new URL(r.url()).pathname === "/fragment") fragmentReqs++;
    });
    // Let any boot-time /fragment (the `load` trigger) settle first.
    await expect(page.getByTestId("trig-load")).toContainText(FRAGMENT_TEXT);
    const before = fragmentReqs;

    const link = page.getByTestId("preload-link");
    await expect
      .poll(
        () =>
          page.evaluate(
            () => (document.getElementById("preload-link") as any)?.preloadState,
          ),
        { timeout: 2000 },
      )
      .toBe("PAUSE");

    await link.hover(); // mouseover → 100ms → doLoad → htmx.ajax GET /fragment
    // preloadState advances to DONE once the warm request resolves.
    await expect
      .poll(
        () =>
          page.evaluate(
            () => (document.getElementById("preload-link") as any)?.preloadState,
          ),
        { timeout: 4000 },
      )
      .toBe("DONE");
    // And a genuine extra /fragment request was issued by the preload.
    expect(fragmentReqs).toBeGreaterThan(before);
  });
});

// ── lifecycle events (DEFECT #1 — keepIndicators panic) ───────────────────────

test.describe("request lifecycle events", () => {
  // keepIndicators is guarded now. On a normal response `keepIndicators` is
  // undefined; the guard treats missing as false (mirroring htmx.js's
  // `!responseInfo.keepIndicators`) instead of panicking on .Bool(). So the
  // post-swap tail runs: removeRequestIndicators, htmx:after:request and
  // htmx:after:request fire, and htmx:error does NOT fire on a healthy
  // response. This asserts that upstream lifecycle.
  test("afterRequest + afterOnLoad fire and onLoadError does not", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.evaluate(() => {
      const w = window as any;
      w.__life = { afterRequest: 0, afterOnLoad: 0, onLoadError: 0 };
      document.addEventListener("htmx:after:request", () => w.__life.afterRequest++);
      document.addEventListener("htmx:after:request", () => w.__life.afterOnLoad++);
      document.addEventListener("htmx:error", () => w.__life.onLoadError++);
    });
    await page.getByTestId("get-basic").click();
    // The swap itself lands regardless (asserted elsewhere); here we require the
    // healthy post-swap tail.
    await expect(page.getByTestId("get-out")).toContainText(FRAGMENT_TEXT);
    const life = await page.evaluate(() => (window as any).__life);
    expect(life.afterRequest, "htmx:after:request fired").toBeGreaterThan(0);
    expect(life.afterOnLoad, "htmx:after:request fired").toBeGreaterThan(0);
    expect(life.onLoadError, "htmx:error did NOT fire").toBe(0);
  });

  // Regression guard: a normal response must NOT emit htmx:error (the
  // keepIndicators panic used to force it). This is the inverse of the retired
  // "current defect" companion and pins the fix.
  test("a normal response does not emit htmx:error", async ({ page }) => {
    await gotoReady(page);
    await page.evaluate(() => {
      const w = window as any;
      w.__onLoadError = 0;
      document.addEventListener("htmx:error", () => w.__onLoadError++);
    });
    await page.getByTestId("get-basic").click();
    await expect(page.getByTestId("get-out")).toContainText(FRAGMENT_TEXT);
    // Give any erroneous onLoadError a chance to fire, then assert it did not.
    await page.waitForTimeout(400);
    expect(await page.evaluate(() => (window as any).__onLoadError)).toBe(0);
  });
});
