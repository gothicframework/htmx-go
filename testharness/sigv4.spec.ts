import { test, expect, Page } from "@playwright/test";
import { createHash } from "node:crypto";

// Validation for the ext/sigv4 in-path AWS SigV4 body-hash signer.
//
// The signer is a Go-native RequestTransformer registered during the WASM boot
// (Install -> sigv4.Register -> preload.Register -> Start), runtime-gated on the
// server-rendered <meta name="gothic-provider" content="AWS"> marker. The /sigv4
// harness page emits that marker only for ?provider=AWS, so the SAME htmx-sigv4
// wasm proves BOTH directions of the gate.
//
// Ground truth is /echo, which reflects the ACTUAL bytes it received:
//   header:x-amz-content-sha256=<value>   the header the signer stamped
//   body=<raw request body>               the exact bytes htmx sent
// so byte-for-byte correctness is asserted empirically:
//   signed hash === sha256hex(the body /echo actually received).

// sha256("") — the empty-body hash the signer sends for useUrlParams (GET/DELETE)
// and no-parameter requests.
const EMPTY_BODY_SHA256 =
  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";

const WASM_TIMEOUT = 20_000;

// readEcho clicks/submits, waits for the scoped echo-payload to render, and
// returns the reflected multi-line string (textContent decodes the HTML entities
// the server escaped, so this is the original body/header text).
async function readEcho(page: Page, outId: string): Promise<string> {
  const payload = page.locator(`#${outId} [data-testid="echo-payload"]`);
  await expect(payload).not.toHaveText("", { timeout: WASM_TIMEOUT });
  return (await payload.textContent())?.trim() ?? "";
}

// field extracts a `name=value` line's value from the reflected echo text. The
// value runs to end-of-line, so `body=a=b&c=d` yields `a=b&c=d` for name "body".
function field(echo: string, name: string): string {
  const m = echo.match(new RegExp(`^${name}=(.*)$`, "m"));
  return m ? m[1] : "__MISSING__";
}

async function bootAWS(page: Page) {
  await page.goto("/sigv4?provider=AWS");
  await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
    timeout: WASM_TIMEOUT,
  });
}

test.describe("ext/sigv4 in-path signer (provider marker = AWS)", () => {
  test.beforeEach(async ({ page }) => {
    page.on("pageerror", (e) => {
      throw new Error(`page error during sigv4 signing: ${e.message}`);
    });
  });

  test("POST urlencoded: header == sha256hex(the exact body /echo received)", async ({
    page,
  }) => {
    await bootAWS(page);
    await page.getByTestId("post-submit").click();
    const echo = await readEcho(page, "post-out");

    const body = field(echo, "body");
    const header = field(echo, "header:x-amz-content-sha256");

    // The body htmx sent, reflected verbatim.
    expect(body).toBe("alpha=one&beta=two");
    // Byte-for-byte: the signed hash is sha256 of exactly those bytes.
    const expected = createHash("sha256").update(body).digest("hex");
    expect(header).toBe(expected);
    expect(header).toMatch(/^[a-f0-9]{64}$/);
  });

  test("GET (useUrlParams): header == EmptyBodyHash", async ({ page }) => {
    await bootAWS(page);
    await page.getByTestId("get-btn").click();
    const echo = await readEcho(page, "get-out");

    expect(field(echo, "method")).toBe("GET");
    // Params travelled in the URL, so the body is empty and the hash is fixed.
    expect(field(echo, "body")).toBe("");
    expect(field(echo, "header:x-amz-content-sha256")).toBe(EMPTY_BODY_SHA256);
  });

  test("multipart: header == UNSIGNED-PAYLOAD", async ({ page }) => {
    await bootAWS(page);
    await page.getByTestId("multipart-submit").click();
    const echo = await readEcho(page, "multipart-out");

    // hx-encoding=multipart/form-data => htmx leaves Content-Type unset (random
    // boundary) => the signer cannot reproduce the body => UNSIGNED-PAYLOAD.
    expect(field(echo, "header:x-amz-content-sha256")).toBe("UNSIGNED-PAYLOAD");
  });

  test("EMPTY multipart: header == UNSIGNED-PAYLOAD (multipart checked before empty)", async ({
    page,
  }) => {
    await bootAWS(page);
    await page.getByTestId("empty-multipart-submit").click();
    const echo = await readEcho(page, "empty-multipart-out");

    // No named controls => EncodedBody()=="" — but the browser still sends the
    // closing boundary bytes, so signing sha256("") would 403. The signer must
    // escape to the sentinel: multipart is checked BEFORE the empty-body branch.
    expect(field(echo, "header:x-amz-content-sha256")).toBe("UNSIGNED-PAYLOAD");
  });
});

test.describe("ext/sigv4 dormant off AWS (no provider marker)", () => {
  test("POST: no x-amz-content-sha256 header is emitted", async ({ page }) => {
    page.on("pageerror", (e) => {
      throw new Error(`page error: ${e.message}`);
    });
    // No ?provider=AWS => no marker => sigv4.Register installs nothing.
    await page.goto("/sigv4");
    await expect(page.locator("body")).toHaveAttribute("data-wasm-ready", "1", {
      timeout: WASM_TIMEOUT,
    });
    await page.getByTestId("post-submit").click();
    const echo = await readEcho(page, "post-out");

    // The body is still sent; the signing header is simply absent (empty value).
    expect(field(echo, "body")).toBe("alpha=one&beta=two");
    expect(field(echo, "header:x-amz-content-sha256")).toBe("");
  });
});
