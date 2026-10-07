// topic-bridge.spec.ts — the Topic ⇄ htmx bridge seams in htmx-go.
//
// Two generic, host-app-agnostic capabilities are exercised here exactly as the
// Gothic core runtime wires them (Gothic registers the "topic:" alias so a Topic
// publish — a `gothic:topic:<name>` CustomEvent dispatched on `document` — drives
// htmx):
//
//   Bridge 1  hx-trigger="topic:<name>"  — an element fires its request when the
//             aliased CustomEvent is dispatched. Proven by registering the alias
//             (htmx.registerTriggerAlias) and dispatching the event by hand (the
//             stand-in for a real Topic publish, which the pure-htmx harness has
//             no runtime for).
//
//   Bridge 2  hx-publish="topic:<name>"  — after a swap, the swapped element's
//             hx-publish value is rewritten through the same alias table and
//             dispatched as a bubbling CustomEvent on document. Proven both
//             directly (a raw listener sees the event) and end-to-end (a Bridge-1
//             subscriber refetches — pure declarative choreography, no JS glue).
//
// The alias table is empty by default, so none of this perturbs the other specs.

import { test, expect, Page } from "@playwright/test";

async function boot(page: Page) {
  await page.goto("/");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 20_000,
  });
  // Register the alias the Gothic core registers in production:
  //   topic:<name>  →  gothic:topic:<name>  (listened on document).
  await page.evaluate(() => {
    (window as any).htmx.registerTriggerAlias("topic:", "gothic:topic:", "document");
  });
}

test("Bridge 1: hx-trigger='topic:x' fires the request when the topic publishes", async ({
  page,
}) => {
  await boot(page);

  await page.evaluate(() => {
    const host = document.createElement("div");
    host.innerHTML =
      `<div id="b1-trigger" hx-get="/fragment" hx-trigger="topic:demo"` +
      ` hx-target="#b1-sink" hx-swap="innerHTML"></div>` +
      `<div id="b1-sink">idle</div>`;
    document.body.appendChild(host);
    // Wire the freshly injected markup (reads hx-trigger, applies the alias).
    (window as any).htmx.process(document.body);
  });

  // Sanity: nothing has fired yet.
  await expect(page.locator("#b1-sink")).toHaveText("idle");

  // A DIFFERENT topic must NOT fire this element (negative control).
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent("gothic:topic:other"))
  );
  await page.waitForTimeout(150);
  await expect(page.locator("#b1-sink")).toHaveText("idle");

  // Publishing the subscribed topic fires the GET and swaps the fragment in.
  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent("gothic:topic:demo"))
  );
  await expect(page.locator("#b1-sink")).toContainText("fragment-ok");
});

test("Bridge 1: an explicit from: still wins over the alias default", async ({
  page,
}) => {
  await boot(page);

  await page.evaluate(() => {
    const host = document.createElement("div");
    // from:document is explicit here; the alias must rewrite the event name but
    // must NOT override the author's from:.
    host.innerHTML =
      `<div id="b1b-trigger" hx-get="/fragment" hx-trigger="topic:scoped from:document"` +
      ` hx-target="#b1b-sink" hx-swap="innerHTML"></div>` +
      `<div id="b1b-sink">idle</div>`;
    document.body.appendChild(host);
    (window as any).htmx.process(document.body);
  });

  await page.evaluate(() =>
    document.dispatchEvent(new CustomEvent("gothic:topic:scoped"))
  );
  await expect(page.locator("#b1b-sink")).toContainText("fragment-ok");
});

test("Bridge 2: hx-publish dispatches the aliased topic event after a swap", async ({
  page,
}) => {
  await boot(page);

  const sawEvent = await page.evaluate(async () => {
    return await new Promise<boolean>((resolve) => {
      const host = document.createElement("div");
      host.innerHTML = `<div id="b2-pub" hx-publish="topic:chain">seed</div>`;
      document.body.appendChild(host);
      (window as any).htmx.process(document.body);

      let fired = false;
      document.addEventListener("gothic:topic:chain", () => {
        fired = true;
      });

      // A swap into #b2-pub fires htmx:after:swap on it; the delegated hx-publish
      // listener rewrites "topic:chain" → "gothic:topic:chain" and dispatches it.
      (window as any).htmx.swap("#b2-pub", "<span>updated</span>", {
        swapStyle: "innerHTML",
      });
      setTimeout(() => resolve(fired), 300);
    });
  });

  expect(sawEvent).toBe(true);
});

test("Bridge 1+2: publish-on-swap chains one region to another through a topic", async ({
  page,
}) => {
  await boot(page);

  await page.evaluate(() => {
    const host = document.createElement("div");
    host.innerHTML =
      // Publisher: broadcasts topic:cart after any swap into it.
      `<div id="b12-pub" hx-publish="topic:cart">seed</div>` +
      // Subscriber: refetches /fragment whenever topic:cart publishes.
      `<div id="b12-sub" hx-get="/fragment" hx-trigger="topic:cart"` +
      ` hx-target="#b12-sink" hx-swap="innerHTML"></div>` +
      `<div id="b12-sink">idle</div>`;
    document.body.appendChild(host);
    (window as any).htmx.process(document.body);
  });

  await expect(page.locator("#b12-sink")).toHaveText("idle");

  // Swap into the publisher — no JS glue: afterSwap → publish topic:cart →
  // subscriber's hx-trigger fires → /fragment swaps into the sink.
  await page.evaluate(() =>
    (window as any).htmx.swap("#b12-pub", "<span>new item</span>", {
      swapStyle: "innerHTML",
    })
  );

  await expect(page.locator("#b12-sink")).toContainText("fragment-ok");
});
