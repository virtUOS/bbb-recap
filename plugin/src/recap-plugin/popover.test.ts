import { describe, expect, it } from "vitest";

import { popoverPosition } from "./popover";

describe("popoverPosition", () => {
  it("opens below the anchor with the right edges aligned", () => {
    expect(popoverPosition({ bottom: 50, right: 900 }, 400, 1280)).toEqual({ top: 58, left: 500, width: 400 });
  });

  it("stays inside the viewport", () => {
    expect(popoverPosition({ bottom: 50, right: 300 }, 400, 1280).left).toBe(8);
    expect(popoverPosition({ bottom: 50, right: 1400 }, 400, 1280).left).toBe(872);
  });

  it("shrinks on a narrow screen", () => {
    expect(popoverPosition({ bottom: 50, right: 360 }, 400, 360)).toEqual({ top: 58, left: 8, width: 344 });
  });

  it("goes top right without an anchor", () => {
    expect(popoverPosition(null, 400, 1280)).toEqual({ top: 64, left: 864, width: 400 });
  });
});
