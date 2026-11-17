import { describe, expect, it } from "vitest";

import { CHIP_DATA_TEST, PILL_DATA_TEST } from "./nav-items";
import { PAGE_CSS } from "./page-style";

describe("PAGE_CSS", () => {
  it("hides exactly BBB's two recording toasts, by their ids", () => {
    expect(PAGE_CSS).toContain(
      '[id="success-record-app.notification.recordingStart"], [id="error-record-app.notification.recordingPaused"] ' +
        "{ position: absolute !important; visibility: hidden !important; pointer-events: none !important; }",
    );
  });

  it("hides BBB's recording pill by its data-test, and nothing else with data-test", () => {
    expect(PAGE_CSS).toContain('[data-test="recordingIndicator"] { display: none !important; }');
    expect(PAGE_CSS.match(/data-test/g)).toHaveLength(1);
  });

  it("does not touch the plugin's own pill or chip", () => {
    expect(PAGE_CSS).not.toContain(PILL_DATA_TEST);
    expect(PAGE_CSS).not.toContain(CHIP_DATA_TEST);
  });

  it("targets nothing else of BBB: no BBB classes, no icons, no broad toast rules", () => {
    expect(PAGE_CSS).not.toMatch(/Toastify|icon-bbb|bbb-|:has\(|recording(?!Indicator|Start|Paused)/);
    const selectors = PAGE_CSS.split("\n").map((rule) => rule.slice(0, rule.indexOf("{")).trim());
    expect(selectors).toEqual([
      '[data-test="recordingIndicator"]',
      '[id="success-record-app.notification.recordingStart"], [id="error-record-app.notification.recordingPaused"]',
      "dialog[data-recap-popover]::backdrop",
    ]);
  });
});
