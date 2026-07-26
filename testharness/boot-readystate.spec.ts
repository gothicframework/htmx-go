import { test, expect, Page } from "@playwright/test";

// Boot-time readyState contract.
//
// A WASM host installs htmx asynchronously, so Install()/Start() can run at any
// document.readyState — including "interactive", the window between
// DOMContentLoaded and load. In that window DOMContentLoaded has already fired
// and will never fire again, so the initialization gate must not park its work
// on that event: everything Start() does (processNode, the indicator styles, the
// abort/cleanup/afterSwap listeners, the popstate override) and every callback
// deferred by htmx.on/htmx.off would be dropped for the page's lifetime.
//
// Determinism, no timing assumptions:
//   - an image request that never resolves keeps the window "load" event from
//     firing, pinning readyState at "interactive" for the whole test;
//   - a delay on htmx.wasm guarantees the boot lands after DOMContentLoaded.

const FRAGMENT_TEXT = "fragment-ok";

async function bootInInteractive(page: Page) {
  // Pin readyState at "interactive": this image never loads, so "load" never fires.
  await page.route("**/__hold-load*", () => {
    /* deliberately never fulfilled */
  });
  // Guarantee the wasm boots after DOMContentLoaded.
  await page.route("**/htmx.wasm", async (route) => {
    await new Promise((r) => setTimeout(r, 400));
    await route.continue();
  });
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => {
      const img = document.createElement("img");
      img.src = "/__hold-load.png";
      img.style.display = "none";
      document.body.appendChild(img);
    });
  });

  await page.goto("/", { waitUntil: "commit" });
  await page.waitForSelector('body[data-wasm-ready="1"]', { timeout: 20_000 });
  // Precondition: the boot really did land in the interactive window.
  expect(await page.evaluate(() => document.readyState)).toBe("interactive");
}

test.describe("boot at readyState=interactive", () => {
  test("Start() runs its full pass — hx-trigger=load fires", async ({ page }) => {
    await bootInInteractive(page);
    await expect(page.getByTestId("trig-load")).toContainText(FRAGMENT_TEXT);
  });

  test("Start() runs its full pass — indicator styles are injected", async ({ page }) => {
    await bootInInteractive(page);
    await expect
      .poll(() =>
        page.evaluate(() =>
          Array.from(document.querySelectorAll("head style")).some((s) =>
            (s.textContent || "").includes("htmx-indicator"),
          ),
        ),
      )
      .toBe(true);
  });

  test("htmx.on attaches immediately instead of deferring", async ({ page }) => {
    await bootInInteractive(page);
    const fired = await page.evaluate(async () => {
      let hits = 0;
      (window as any).htmx.on(document.body, "gothic:readystate-probe", () => {
        hits++;
      });
      document.body.dispatchEvent(new CustomEvent("gothic:readystate-probe"));
      return hits;
    });
    expect(fired).toBe(1);
  });
});
