import { test, expect, Page } from "@playwright/test";

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

// Reproduces the exact Gothic wrapper path:
//   HTMX.On(subtree)   -> htmx.on("[data-gothic-scope=X]", event, fn)   (3-arg)
//   HTMX.OnGlobal      -> htmx.on(event, fn)                            (2-arg, body)
//   HTMX.Swap          -> htmx.swap("#target", html, {swapStyle})       (programmatic)
//   HTMX.Ajax          -> htmx.ajax("GET", url, {target})               (programmatic)
test("3-arg subtree on + programmatic swap: does afterSwap reach subtree + global listeners", async ({
  page,
}) => {
  const errors = await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      const hx = (window as any).htmx;
      // Build a scope root with a swap target INSIDE it.
      const root = document.createElement("div");
      root.setAttribute("data-gothic-scope", "S1");
      root.innerHTML = `<div id="swap-target">orig</div>`;
      document.body.appendChild(root);

      const counts = { subtree: 0, global: 0 };
      // 3-arg subtree form (exactly HTMX.On)
      hx.on('[data-gothic-scope="S1"]', "htmx:after:swap", () => counts.subtree++);
      // 2-arg body form (exactly HTMX.OnGlobal)
      hx.on("htmx:after:swap", () => counts.global++);

      // programmatic swap (exactly HTMX.Swap)
      hx.swap("#swap-target", "<span id='swapped'>new</span>", { swapStyle: "innerHTML" });

      setTimeout(() => resolve(counts), 500);
    });
  });
  console.log("SWAP RESULT", JSON.stringify(result));
  expect(result.global, "OnGlobal (body) afterSwap").toBeGreaterThan(0);
  expect(result.subtree, "On (subtree scope-root) afterSwap").toBeGreaterThan(0);
  expect(errors, errors.join("\n")).toHaveLength(0);
});

test("3-arg subtree on + programmatic ajax: afterSwap reaches listeners", async ({
  page,
}) => {
  const errors = await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      const hx = (window as any).htmx;
      const root = document.createElement("div");
      root.setAttribute("data-gothic-scope", "S2");
      root.innerHTML = `<div id="ajax-target">orig</div>`;
      document.body.appendChild(root);

      const counts = { subtree: 0, global: 0 };
      hx.on('[data-gothic-scope="S2"]', "htmx:after:swap", () => counts.subtree++);
      hx.on("htmx:after:swap", () => counts.global++);

      hx.ajax("GET", "/fragment", { target: "#ajax-target" });
      setTimeout(() => resolve(counts), 1500);
    });
  });
  console.log("AJAX RESULT", JSON.stringify(result));
  expect(result.global, "OnGlobal afterSwap via ajax").toBeGreaterThan(0);
  expect(result.subtree, "On subtree afterSwap via ajax").toBeGreaterThan(0);
  expect(errors, errors.join("\n")).toHaveLength(0);
});

test("trigger with detail via selector target reaches 2-arg document on", async ({
  page,
}) => {
  const errors = await boot(page);
  const received = await page.evaluate(() => {
    return new Promise<string>((resolve) => {
      const hx = (window as any).htmx;
      const t = document.createElement("div");
      t.id = "trig-target";
      document.body.appendChild(t);
      hx.on("gothic:ping", (e: any) => resolve((e && e.detail && e.detail.msg) || "no-detail"));
      hx.trigger("#trig-target", "gothic:ping", { msg: "pong" });
      setTimeout(() => resolve("TIMEOUT"), 2000);
    });
  });
  console.log("TRIGGER RESULT", received);
  expect(received).toBe("pong");
  expect(errors, errors.join("\n")).toHaveLength(0);
});
