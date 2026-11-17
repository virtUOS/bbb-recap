import { describe, expect, it } from "vitest";

import { messagesFor } from "./messages";
import {
  CHIP_ID,
  type NavBarButtonProps,
  PILL_ID,
  chipItem,
  navView,
  pillItem,
  type PillInput,
  popoverOnClick,
  popoverStillFits,
} from "./nav-items";
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
  return svgOfItem(pillItem(input));
}

function svgOfItem(item: NavBarButtonProps) {
  const icon = item.icon;
  if (typeof icon !== "object" || !("svgContent" in icon)) {
    throw new Error("no svg icon");
  }
  return icon.svgContent;
}

describe("pillItem", () => {
  it("off: a start action without a timer", () => {
    const item = pillItem(base);
    expect(textOf(item.label)).toBe("Start AI minutes");
    expect(item.tooltip).toBe("Start AI minutes. They are off.");
    expect(item.style?.border).toBe("1px solid transparent");
  });

  it("running: label with the active time, solid violet border", () => {
    const item = pillItem({ ...base, state: "running" });
    expect(textOf(item.label)).toBe("AI minutes· 12:04");
    expect(item.tooltip).toBe("AI minutes running. Click to pause.");
    expect(item.style?.border).toContain("solid");
    expect(item.style?.background).toContain("color-mix");
  });

  it("paused: label with the time, dashed border, resume in the tooltip", () => {
    const item = pillItem({ ...base, state: "paused" });
    expect(textOf(item.label)).toBe("AI minutes paused· 12:04");
    expect(item.tooltip).toBe("AI minutes paused. Click to resume.");
    expect(item.style?.border).toContain("dashed");
  });

  it("starts the tooltip, which is the accessible name, with the visible label text (WCAG 2.5.3)", () => {
    for (const locale of ["en", "de"]) {
      const messages = messagesFor(locale);
      for (const state of ["off", "running", "paused"] as const) {
        const item = pillItem({ ...base, state, messages });
        const visible = textOf(item.label).split("·")[0].trim();
        expect(item.tooltip.startsWith(visible)).toBe(true);
      }
      for (const state of ["running", "paused"] as const) {
        const item = chipItem({ state, messages, reducedMotion: false, onClick: () => {} });
        expect(item.tooltip.startsWith(textOf(item.label))).toBe(true);
      }
    }
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

describe("popovers of the pill", () => {
  it("start from off, pause from running, none from paused (resume at once)", () => {
    expect(popoverOnClick("off")).toBe("start");
    expect(popoverOnClick("running")).toBe("pause");
    expect(popoverOnClick("paused")).toBeNull();
  });

  it("close when the state changes under them or the user loses the control", () => {
    expect(popoverStillFits("start", "pill", "off")).toBe(true);
    expect(popoverStillFits("start", "pill", "running")).toBe(false);
    expect(popoverStillFits("pause", "pill", "running")).toBe(true);
    expect(popoverStillFits("pause", "pill", "paused")).toBe(false);
    expect(popoverStillFits("start", "chip", "off")).toBe(false);
    expect(popoverStillFits("start", null, "off")).toBe(false);
  });

  it("info stays open while the chip shows, in both states", () => {
    expect(popoverStillFits("info", "chip", "running")).toBe(true);
    expect(popoverStillFits("info", "chip", "paused")).toBe(true);
    expect(popoverStillFits("info", "pill", "running")).toBe(false);
    expect(popoverStillFits("info", null, "off")).toBe(false);
  });
});

describe("navView", () => {
  it("shows the pill to users with the control, in every state", () => {
    for (const state of ["off", "running", "paused"] as const) {
      expect(navView(true, true, state)).toBe("pill");
    }
  });

  it("shows the chip to everyone else, hidden while off", () => {
    expect(navView(true, false, "off")).toBeNull();
    expect(navView(true, false, "running")).toBe("chip");
    expect(navView(true, false, "paused")).toBe("chip");
  });

  it("shows nothing while loading", () => {
    expect(navView(false, false, "running")).toBeNull();
    expect(navView(false, true, "off")).toBeNull();
  });
});

describe("chipItem", () => {
  const chip = { messages: messagesFor("en"), reducedMotion: false, onClick: () => {} };

  it("running: label, details in the tooltip, pulsing dot", () => {
    const item = chipItem({ ...chip, state: "running" });
    expect(textOf(item.label)).toBe("Minutes are being taken");
    expect(item.tooltip).toBe("Minutes are being taken. Click for details.");
    expect(item.id).toBe(CHIP_ID);
    expect(item.style?.height).toBe("2rem");
    expect(item.disabled).toBe(false);
    expect(elementsOf(svgOfItem(item), "animate")).toHaveLength(2);
  });

  it("paused: hollow dot without pulse", () => {
    const item = chipItem({ ...chip, state: "paused" });
    expect(textOf(item.label)).toBe("Minutes paused");
    expect(elementsOf(svgOfItem(item), "animate")).toHaveLength(0);
    expect(elementsOf(svgOfItem(item), "circle")[0].fill).toBe("none");
  });

  it("does not pulse with prefers-reduced-motion", () => {
    const item = chipItem({ ...chip, state: "running", reducedMotion: true });
    expect(elementsOf(svgOfItem(item), "animate")).toHaveLength(0);
  });

  it("is German for a German locale", () => {
    const item = chipItem({ ...chip, state: "running", messages: messagesFor("de") });
    expect(textOf(item.label)).toBe("Wird protokolliert");
  });
});
