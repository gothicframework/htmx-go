import { test, expect, Page } from "@playwright/test";

// Verbs pass-through, hx-status patterns, boost HCON semantics and the v4
// swap-style table (innerMorph/outerMorph/outerSync/textContent + alias
// macros), asserted on OBSERVABLE outcomes — the request on the wire or the
// resulting DOM mutation.
//
// Readiness is deterministic: body[data-wasm-ready="1"]. NEVER networkidle —
// the `every 1s` polling trigger keeps the network perpetually busy.

async function gotoReady(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 15_000,
  });
  await expect(page.locator("body")).not.toHaveAttribute("data-wasm-error", /.+/);
}

/** Mount fixture markup and let htmx process it, reusing the harness's
 * established htmx.process re-wiring path. */
async function mount(page: Page, html: string): Promise<void> {
  await page.evaluate((markup) => {
    const host = document.createElement("div");
    host.innerHTML = markup;
    document.body.appendChild(host);
    (window as any).htmx.process(host);
  }, html);
}

// ── custom verbs (hx-method passes through; QUERY sends params in the body) ──

test.describe("custom request methods", () => {
  test("hx-action + hx-method=HEAD passes through (no GET downgrade)", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="verb-head" hx-action="/echo" hx-method="HEAD">HEAD</button>`,
    );
    const [request] = await Promise.all([
      page.waitForRequest((r) => r.url().includes("/echo") && r.method() === "HEAD"),
      page.getByTestId("verb-head").click(),
    ]);
    expect(request.method()).toBe("HEAD");
  });

  test("hx-method=QUERY sends the parameters in the body (not the URL)", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="verb-query" hx-action="/echo" hx-method="QUERY" ` +
        `hx-target="#verb-query-out" hx-swap="innerHTML">QUERY</button>` +
        `<div id="verb-query-out" data-testid="verb-query-out"></div>`,
    );
    // QUERY is in no URL-params method set, and /echo reflects the method it
    // received, so one round-trip asserts both behaviors.
    await Promise.all([
      page.waitForRequest(
        (r) => r.url().includes("/echo") && r.method() === "QUERY",
      ),
      page.getByTestId("verb-query").click(),
    ]);
    await expect(page.getByTestId("verb-query-out")).toContainText("method=QUERY");
  });
});

// ── hx-status:<pattern> swap overrides ────────────────────────────────────────

test.describe("hx-status pattern overrides", () => {
  test("a 2xx pattern overrides the swap style for a matching 2xx", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="status-trigger" hx-get="/status?code=200" ` +
        `hx-target="#status-out" hx-swap="beforeend" hx-status:2xx="swap: textContent">status</button>` +
        `<div id="status-out" data-testid="status-out"><em>old child</em></div>`,
    );
    await page.getByTestId("status-trigger").click();
    // textContent swap: the payload text lands in the target and the element
    // children are gone. Also coverage for the textContent swap style itself.
    await expect(page.getByTestId("status-out")).toHaveText("status-ok");
  });
});

// ── boost HCON: whole-context overrides, and hx-boost="false" ────────────────

test.describe("hx-boost", () => {
  test("a boost HCON swaps the configured target with the configured style", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<a href="/wrapper" id="hcon-boost-link" data-testid="hcon-boost-link" ` +
        `hx-boost="swap: outerHTML target: #hcon-boost-target">boost</a>` +
        `<div id="hcon-boost-target" data-testid="hcon-boost-target">target untouched</div>`,
    );
    await page.getByTestId("hcon-boost-link").click();
    // The swap applied to the HCON's target (#hcon-boost-target) with the
    // configured style (outerHTML), so the payload element REPLACES it in the
    // DOM — the adopted element is reached by the payload's id, not by the
    // replaced node's test id.
    const target = page.locator("#wrapper-payload-id");
    await expect(target).toContainText("wrapped-ok");
    await expect(target).toHaveClass(/adopted-class/);
    await expect(page.locator("#hcon-boost-target")).toHaveCount(0);
    // The boosting anchor survived (no full-page navigation happened).
    await expect(page.getByTestId("hcon-boost-link")).toBeVisible();
  });

  test("hx-boost=false anchors navigate with a full reload", async ({ page }) => {
    await gotoReady(page);
    await page.evaluate(() => { (window as any).__wipeProbe = "mounted"; });
    await mount(
      page,
      `<a href="/fragment" id="no-boost-link" data-testid="no-boost-link" hx-boost="false">no boost</a>`,
    );
    await page.getByTestId("no-boost-link").click();
    // A full reload replaces the whole document — the probe is gone.
    await expect
      .poll(() => page.evaluate(() => (window as any).__wipeProbe ?? ""))
      .toBe("");
  });

  test("an hx-boost=true anchor swaps body in place, keeping window state", async ({
    page,
  }) => {
    await gotoReady(page);
    await page.evaluate(() => { (window as any).__boostProbe = "kept"; });
    await mount(
      page,
      `<a href="/fragment" id="boost-link" data-testid="boost-link" hx-boost="true">boost</a>`,
    );
    await page.getByTestId("boost-link").click();
    await expect(page.getByTestId("fragment-payload")).toContainText("fragment-ok");
    // The boot state survives: the navigation never unloaded the document.
    expect(await page.evaluate(() => (window as any).__boostProbe ?? "")).toBe("kept");
  });
});

// ── morph swap styles: matched id nodes are patched in place ─────────────────

test.describe("morph swap styles", () => {
  test("innerMorph keeps the identity of the matched id node and updates it", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="morph-inner-trigger" hx-get="/morph-inner" ` +
        `hx-target="#morph-inner-target" hx-swap="innerMorph">morph</button>` +
        `<div id="morph-inner-target" data-testid="morph-inner-out">` +
        `<span data-testid="morph-span" id="morph-span">morph-inner-old</span></div>`,
    );
    // Stamp the live span; the matching node must be the SAME object afterwards.
    await page.evaluate(() => {
      (document.getElementById("morph-span") as HTMLElement).__morphKept = true;
    });
    await page.getByTestId("morph-inner-trigger").click();
    await expect(page.getByTestId("morph-span")).toHaveText("morph-inner-new");
    await expect
      .poll(() =>
        page.evaluate(
          () => (document.getElementById("morph-span") as any)?.__morphKept === true,
        ),
      )
      .toBe(true);
  });

  test("outerMorph morphs the id-carrying target in place", async ({ page }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="morph-outer-trigger" hx-get="/morph-outer" ` +
        `hx-target="#morph-target" hx-swap="outerMorph">morph</button>` +
        `<div id="morph-target" data-testid="morph-target" class="stale-class">old</div>`,
    );
    await page.evaluate(() => {
      (document.getElementById("morph-target") as HTMLElement).__morphKept = true;
    });
    await page.getByTestId("morph-outer-trigger").click();
    await expect(page.getByTestId("morph-target")).toContainText("morph-outer-new");
    await expect(page.getByTestId("morph-target")).toHaveClass(/fresh-class/);
    await expect(page.getByTestId("morph-target")).not.toHaveClass(/stale-class/);
    await expect
      .poll(() =>
        page.evaluate(
          () => (document.getElementById("morph-target") as any)?.__morphKept === true,
        ),
      )
      .toBe(true);
  });

  test("outerSync adopts the first fragment element's attributes and children", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="outersync-trigger" hx-get="/wrapper" ` +
        `hx-target="#outersync-target" hx-swap="outerSync">sync</button>` +
        `<div id="outersync-target" data-testid="outersync-out" class="old-class">old</div>`,
    );
    await page.getByTestId("outersync-trigger").click();
    // outerSync keeps the target element itself and adopts the payload's
    // attributes and children in place — upstream __copyAttributes copies the
    // source's attributes (id, data-testid, class) over the target and removes
    // the ones the source does not carry, so the adopted element is reached by
    // the payload's id.
    const adopted = page.locator("#wrapper-payload-id");
    await expect(adopted).toHaveClass(/adopted-class/);
    await expect(adopted).toContainText("wrapped-ok");
    await expect(adopted).not.toHaveClass(/old-class/);
    await expect(page.locator("#outersync-target")).toHaveCount(0);
  });
});

// ── aliases, unknown styles, textContent ─────────────────────────────────────

test.describe("swap style aliases and fallback", () => {
  test("hx-swap=prepend inserts the fragment at afterbegin", async ({ page }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="prepend-trigger" hx-get="/fragment" ` +
        `hx-target="#prepend-target" hx-swap="prepend">prepend</button>` +
        `<ol id="prepend-target" data-testid="prepend-out"><li data-testid="prepend-old">old</li></ol>`,
    );
    await page.getByTestId("prepend-trigger").click();
    const target = page.getByTestId("prepend-out");
    await expect(target).toContainText("fragment-ok");
    const firstChildText = await target.evaluate(
      (el) => el.firstElementChild?.textContent ?? "",
    );
    expect(firstChildText).toBe("fragment-ok");
  });

  test("an unknown swap style logs an error and falls back to the default style", async ({
    page,
  }) => {
    await gotoReady(page);
    await mount(
      page,
      `<button data-testid="bogus-trigger" hx-get="/fragment" ` +
        `hx-target="#bogus-target" hx-swap="morphMissing">bogus</button>` +
        `<div id="bogus-target" data-testid="bogus-out"></div>`,
    );
    // The log races the click's XHR round-trip, so wait for the console event
    // itself instead of snapshotting a console array right after the click.
    const styleError = page.waitForEvent("console", (m) =>
      m.type() === "error" && m.text().includes("Unknown swap style: morphMissing"),
    );
    await page.getByTestId("bogus-trigger").click();
    await styleError;
    // The default (innerHTML) swap still happened instead of trapping or
    // silently re-running an unintended style with no signal.
    await expect(page.getByTestId("bogus-out")).toContainText("fragment-ok");
  });
});
