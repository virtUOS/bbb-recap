import { describe, expect, it } from "vitest";

import { messagesFor } from "./messages";

describe("messagesFor", () => {
  it("picks German for de and its regions", () => {
    expect(messagesFor("de").labelOff).toBe("KI-Zusammenfassung: aus");
    expect(messagesFor("de-DE").labelOn).toBe("KI-Zusammenfassung: an");
    expect(messagesFor("de_AT").start).toBe("Starten");
  });

  it("falls back to English", () => {
    expect(messagesFor("en-US").labelOff).toBe("AI summary: off");
    expect(messagesFor("pt-BR").labelOn).toBe("AI summary: on");
    expect(messagesFor("").cancel).toBe("Cancel");
    expect(messagesFor(undefined).cancel).toBe("Cancel");
  });

  it("has the same keys in every language", () => {
    expect(Object.keys(messagesFor("de")).sort()).toEqual(Object.keys(messagesFor("en")).sort());
  });
});
