import { describe, expect, it } from "vitest";

import { messagesFor } from "./messages";
import { PILL_ID, pillItem, type PillInput } from "./nav-items";
import { elementsOf, textOf } from "./test-utils";

const base: PillInput = {
  state: "off",
  seconds: 724,
  messages: messagesFor("en"),
  reducedMotion: false,
  disabled: false,
  onClick: () => {},
};

function svgOf(input: PillInput) {
  const icon = pillItem(input).icon;
  if (typeof icon !== "object" || !("svgContent" in icon)) {
    throw new Error("no svg icon");
  }
  return icon.svgContent;
}

describe("pillItem", () => {
  it("off: a start action without a timer", () => {
    const item = pillItem(base);
    expect(textOf(item.label)).toBe("Start AI minutes");
    expect(item.tooltip).toBe("AI minutes are off. Click to start.");
    expect(item.style?.border).toBe("1px solid transparent");
  });

  it("running: label with the active time, solid violet border", () => {
    const item = pillItem({ ...base, state: "running" });
    expect(textOf(item.label)).toBe("AI minutes· 12:04");
    expect(item.tooltip).toBe("AI minutes are running. Click to pause.");
    expect(item.style?.border).toContain("solid");
    expect(item.style?.background).toContain("color-mix");
  });

  it("paused: label with the time, dashed border, resume in the tooltip", () => {
    const item = pillItem({ ...base, state: "paused" });
    expect(textOf(item.label)).toBe("AI minutes paused· 12:04");
    expect(item.tooltip).toBe("AI minutes are paused. Click to resume.");
    expect(item.style?.border).toContain("dashed");
  });

  it("keeps the tooltip free of the timer, since it is the aria-label", () => {
    for (const state of ["off", "running", "paused"] as const) {
      expect(pillItem({ ...base, state }).tooltip).not.toMatch(/\d/);
    }
  });

  it("has the same id, colour and box in every state", () => {
    for (const state of ["off", "running", "paused"] as const) {
      const item = pillItem({ ...base, state });
      expect(item.id).toBe(PILL_ID);
      expect(item.color).toBe("default");
      expect(item.style?.height).toBe("2.25rem");
      expect(item.hasSeparator).toBe(false);
    }
  });

  it("is German for a German locale", () => {
    const item = pillItem({ ...base, state: "paused", messages: messagesFor("de") });
    expect(textOf(item.label)).toBe("KI-Protokoll pausiert· 12:04");
    expect(item.tooltip).toBe("KI-Protokoll pausiert. Klicken zum Fortsetzen.");
  });
});

describe("pill icon motion", () => {
  it("pulses only while running", () => {
    expect(elementsOf(svgOf({ ...base, state: "running" }), "animate")).toHaveLength(2);
    expect(elementsOf(svgOf({ ...base, state: "paused" }), "animate")).toHaveLength(0);
    expect(elementsOf(svgOf({ ...base, state: "off" }), "animate")).toHaveLength(0);
  });

  it("does not pulse with prefers-reduced-motion", () => {
    expect(elementsOf(svgOf({ ...base, state: "running", reducedMotion: true }), "animate")).toHaveLength(0);
  });
});
