import { test, expect, Page } from "@playwright/test";

// Regression coverage for three port-fidelity defects the full Gothic e2e gate
// surfaced and that were fixed in htmx.go. Each reproduces the failure in
// isolation against the compiled WASM port so it can never silently regress:
//
//   Cluster A — formDataFromObject panicked on a STRING hx-vals value.
//     `hx-vals='{"id":"x"}'` parses to {id:"x"}; the string VALUE "x" then hit
//     `val.Get("forEach")` (js.Value.Get on a string PANICS, killing core.wasm).
//     htmx.js does the forgiving `obj[key].forEach` → harmless undefined. Covered
//     as JSON-string, js:-expression, and programmatic-object values, on GET+POST,
//     asserting the value reaches the request (params on GET, body on POST) with
//     ZERO console errors.
//
//   Cluster B — htmx.on / htmx.trigger event dispatch. A custom event registered
//     via the public API must be received; htmx:afterSwap must fire and a listener
//     must observe it after a real swap.
//
//   Cluster C — parseJSON("") logged `htmx: invalid JSON` to console.error on the
//     first history save (empty sessionStorage → jstr(null) → ""), where htmx.js's
//     JSON.parse(null) yields null silently. A push-url request must not log it.

const READY = 'body[data-wasm-ready="1"]';

async function boot(page: Page): Promise<string[]> {
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(`console.error: ${m.text()}`);
  });
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  await page.goto("/");
  await page.waitForSelector(READY, { timeout: 20_000 });
  return errors;
}

// runtimeAlive proves the Go/WASM runtime did NOT die (a panic in the boot/request
// path parks the runtime and every later window.htmx.* call throws
// "Go program has already exited").
async function runtimeAlive(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    try {
      return typeof (window as any).htmx.version === "string";
    } catch {
      return false;
    }
  });
}

// fireHxVals injects an htmx-driven element, processes it so the port wires the
// hx-* attributes, clicks it, and returns the swapped-in /echo reflection text
// PLUS the URL of the /echo request it fired. GET carries hx-vals in the query
// string (asserted via reqUrl); POST carries them in the body (echoed in `text`).
async function fireHxVals(
  page: Page,
  attrs: string,
  outId: string
): Promise<{ text: string; reqUrl: string }> {
  let reqUrl = "";
  page.on("request", (r) => {
    if (r.url().includes("/echo")) reqUrl = r.url();
  });
  await page.evaluate(
    ([a, id]) => {
      const host = document.createElement("div");
      host.innerHTML =
        `<button id="btn-${id}" ${a} hx-target="#${id}" hx-swap="innerHTML"></button>` +
        `<div id="${id}"></div>`;
      document.body.appendChild(host);
      (window as any).htmx.process(host);
    },
    [attrs, outId]
  );
  await page.click(`#btn-${outId}`);
  await expect(page.locator(`#${outId}`)).toContainText("method=", {
    timeout: 8_000,
  });
  return { text: await page.locator(`#${outId}`).innerText(), reqUrl };
}

test.describe("Cluster A — hx-vals never panics and reaches the request", () => {
  test("JSON-string hx-vals on GET → query param", async ({ page }) => {
    const errors = await boot(page);
    const out = await fireHxVals(
      page,
      `hx-get="/echo" hx-vals='{"id":"getjson"}'`,
      "a-getjson"
    );
    expect(out.text).toContain("method=GET");
    expect(out.reqUrl).toContain("id=getjson");
    expect(await runtimeAlive(page)).toBe(true);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });

  test("JSON-string hx-vals on POST → request body", async ({ page }) => {
    const errors = await boot(page);
    const out = await fireHxVals(
      page,
      `hx-post="/echo" hx-vals='{"id":"postjson"}'`,
      "a-postjson"
    );
    expect(out.text).toContain("method=POST");
    expect(out.text).toContain("param:id=postjson");
    expect(out.text).toContain("body=id=postjson");
    expect(await runtimeAlive(page)).toBe(true);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });

  test("js:-expression hx-vals on GET → query param", async ({ page }) => {
    const errors = await boot(page);
    const out = await fireHxVals(
      page,
      `hx-get="/echo" hx-vals='js:{id: "get" + "js"}'`,
      "a-getjs"
    );
    expect(out.text).toContain("method=GET");
    expect(out.reqUrl).toContain("id=getjs");
    expect(await runtimeAlive(page)).toBe(true);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });

  test("js:-expression hx-vals on POST → request body", async ({ page }) => {
    const errors = await boot(page);
    const out = await fireHxVals(
      page,
      `hx-post="/echo" hx-vals='js:{id: "post" + "js"}'`,
      "a-postjs"
    );
    expect(out.text).toContain("method=POST");
    expect(out.text).toContain("param:id=postjs");
    expect(await runtimeAlive(page)).toBe(true);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });

  test("programmatic object values via htmx.ajax on POST → request body", async ({
    page,
  }) => {
    const errors = await boot(page);
    await page.evaluate(() => {
      const o = document.createElement("div");
      o.id = "a-ajaxobj";
      document.body.appendChild(o);
      (window as any).htmx.ajax("POST", "/echo", {
        target: "#a-ajaxobj",
        values: { id: "ajaxobj" },
      });
    });
    await expect(page.locator("#a-ajaxobj")).toContainText("method=POST", {
      timeout: 8_000,
    });
    const out = await page.locator("#a-ajaxobj").innerText();
    expect(out).toContain("param:id=ajaxobj");
    expect(await runtimeAlive(page)).toBe(true);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });
});

test.describe("Cluster B — htmx.on / htmx.trigger event dispatch", () => {
  test("a custom event dispatched via htmx.trigger reaches an htmx.on listener", async ({
    page,
  }) => {
    const errors = await boot(page);
    const received = await page.evaluate(() => {
      return new Promise<string>((resolve) => {
        const hx = (window as any).htmx;
        hx.on("gothic:ping", (e: any) => resolve((e && e.detail && e.detail.msg) || "pong"));
        // selector-string target (exactly how the Gothic wrapper calls trigger)
        hx.trigger("body", "gothic:ping", { msg: "pong" });
        setTimeout(() => resolve("TIMEOUT"), 3000);
      });
    });
    expect(received).toBe("pong");
    expect(errors, errors.join("\n")).toHaveLength(0);
  });

  test("htmx:afterSwap fires and a listener count increments after a swap", async ({
    page,
  }) => {
    const errors = await boot(page);
    await page.evaluate(() => {
      (window as any).__afterSwap = 0;
      (window as any).htmx.on("htmx:afterSwap", () => {
        (window as any).__afterSwap++;
      });
      const host = document.createElement("div");
      host.innerHTML =
        `<button id="b-swap" hx-get="/fragment" hx-target="#b-swap-out" hx-swap="innerHTML"></button>` +
        `<div id="b-swap-out">orig</div>`;
      document.body.appendChild(host);
      (window as any).htmx.process(host);
    });
    const before = await page.evaluate(() => (window as any).__afterSwap);
    await page.click("#b-swap");
    await expect(page.locator("#b-swap-out")).not.toHaveText("orig", {
      timeout: 8_000,
    });
    const after = await page.evaluate(() => (window as any).__afterSwap);
    expect(after).toBeGreaterThan(before);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });
});

test.describe("Cluster C — history save does not log invalid JSON", () => {
  test("a push-url request with empty history cache logs no 'invalid JSON'", async ({
    page,
  }) => {
    const errors = await boot(page);
    // Empty the history cache so saveToHistoryCache reads a null/absent item —
    // the exact state that made parseJSON("") throw and log console.error.
    await page.evaluate(() => sessionStorage.removeItem("htmx-history-cache"));
    await page.evaluate(() => {
      const host = document.createElement("div");
      host.innerHTML =
        `<button id="c-push" hx-get="/fragment" hx-push-url="/pushed-path"` +
        ` hx-target="#c-out" hx-swap="innerHTML"></button><div id="c-out"></div>`;
      document.body.appendChild(host);
      (window as any).htmx.process(host);
    });
    await page.click("#c-push");
    await expect(page.locator("#c-out")).toContainText("fragment", {
      timeout: 8_000,
    });
    await page.waitForTimeout(300);
    expect(await runtimeAlive(page)).toBe(true);
    expect(
      errors.filter((e) => e.includes("invalid JSON")),
      `invalid JSON logged:\n${errors.join("\n")}`
    ).toHaveLength(0);
    expect(errors, errors.join("\n")).toHaveLength(0);
  });
});
