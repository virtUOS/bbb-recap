import { describe, expect, it } from "vitest";

import { PAGE_CSS } from "./page-style";

describe("PAGE_CSS", () => {
  it("hides exactly BBB's two recording toasts, by their ids", () => {
    expect(PAGE_CSS).toContain(
      '[id="success-record-app.notification.recordingStart"], [id="error-record-app.notification.recordingPaused"] ' +
        "{ display: none !important; }",
    );
  });

  it("targets nothing else of BBB: no BBB classes, no data-test, no broad toast rules", () => {
    expect(PAGE_CSS).not.toMatch(/Toastify|icon-bbb|data-test|bbb-|:has\(/);
    const selectors = PAGE_CSS.split("\n").map((rule) => rule.slice(0, rule.indexOf("{")).trim());
    for (const selector of selectors) {
      expect(selector).toMatch(/^(\[id="[^"]+"\](, )?)+$|^dialog\[data-recap-popover\]::backdrop$/);
    }
  });
});
