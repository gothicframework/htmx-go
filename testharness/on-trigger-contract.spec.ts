// on-trigger-contract.spec.ts — htmx-go public event-API contract coverage.
//
// Locks in the three window.htmx event-API shapes the Gothic HTMX wrapper
// (core/wasm/wasm-runtime/runtime/htmx.go: HTMX.On / HTMX.OnGlobal / HTMX.Trigger)
// relies on. The wrapper binds listeners via 2-arg `htmx.on(event, fn)` (document-
// wide / OnGlobal) and 3-arg `htmx.on(selector, event, fn)` (subtree / On), and
// fires events via `htmx.trigger(target, name, detail)`. If htmx-go's on/off/trigger
// ever regresses on any of these, an On/OnGlobal listener would bind but never fire
// (exactly the Cluster-B symptom), so this pins the contract at the port level.

import { test, expect, Page } from "@playwright/test";

async function boot(page: Page) {
  await page.goto("/");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: 20_000,
  });
}

test("2-arg htmx.on(event, listener) receives a bubbling trigger fired deep in the tree", async ({
  page,
}) => {
  await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      let fired = 0;
      let gotDetail: any = null;
      // Document-wide listener via the 2-arg form (OnGlobal's binding shape).
      (window as any).htmx.on("gothic:ping", (e: any) => {
        fired++;
        gotDetail = e && e.detail ? e.detail.msg : null;
      });
      // Fire on a node deep in the tree; the event must bubble up to document.
      const deep = document.querySelector("#get-out");
      (window as any).htmx.trigger(deep, "gothic:ping", { msg: "pong" });
      setTimeout(() => resolve({ fired, gotDetail }), 100);
    });
  });
  expect(result.fired).toBeGreaterThan(0);
  expect(result.gotDetail).toBe("pong");
});

test("3-arg htmx.on(selector, event, listener) receives its subtree's events", async ({
  page,
}) => {
  await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      let fired = 0;
      // Subtree-scoped listener on #echo-form (On's binding shape).
      (window as any).htmx.on("#echo-form", "gothic:sub", () => {
        fired++;
      });
      // Fire the event on a child input inside #echo-form; it must bubble to the
      // subtree root the listener is attached to.
      const child = document.querySelector('#echo-form input[name="alpha"]');
      (window as any).htmx.trigger(child, "gothic:sub", {});
      setTimeout(() => resolve({ fired }), 100);
    });
  });
  expect(result.fired).toBeGreaterThan(0);
});

test("htmx.trigger detail round-trips to a document-level htmx.on listener", async ({
  page,
}) => {
  await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      let detail: any = null;
      (window as any).htmx.on("gothic:detail", (e: any) => {
        detail = e && e.detail ? { a: e.detail.a, n: e.detail.n } : null;
      });
      (window as any).htmx.trigger(document.body, "gothic:detail", { a: "x", n: 42 });
      setTimeout(() => resolve({ detail }), 100);
    });
  });
  expect(result.detail).toEqual({ a: "x", n: 42 });
});

test("2-arg htmx.on receives a real bubbling htmx:after:swap event from htmx.swap", async ({
  page,
}) => {
  await boot(page);
  const result = await page.evaluate(() => {
    return new Promise<any>((resolve) => {
      let fired = 0;
      (window as any).htmx.on("htmx:after:swap", () => {
        fired++;
      });
      // htmx.swap fires htmx:after:swap on the target; it must bubble to document.
      (window as any).htmx.swap(
        "#get-out",
        "<span id='swapped-marker'>x</span>",
        { swapStyle: "innerHTML" }
      );
      setTimeout(() => resolve({ fired }), 300);
    });
  });
  expect(result.fired).toBeGreaterThan(0);
});
