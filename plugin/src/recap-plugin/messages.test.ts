import { describe, expect, it } from "vitest";

import { messagesFor } from "./messages";

describe("messagesFor", () => {
  it("picks German for de and its regions", () => {
    expect(messagesFor("de").pillStart).toBe("KI-Protokoll starten");
    expect(messagesFor("de-DE").pillRunning).toBe("KI-Protokoll");
    expect(messagesFor("de_AT").startButton).toBe("Starten");
  });

  it("uses English as the base for every other locale", () => {
    expect(messagesFor("en-US").pillStart).toBe("Start AI minutes");
    expect(messagesFor("pt-BR").pillRunning).toBe("AI minutes");
    expect(messagesFor("").cancel).toBe("Cancel");
    expect(messagesFor(undefined).cancel).toBe("Cancel");
  });

  it("has the same keys in every language, none empty", () => {
    const en = messagesFor("en");
    const de = messagesFor("de");
    expect(Object.keys(de).sort()).toEqual(Object.keys(en).sort());
    for (const text of [...Object.values(en), ...Object.values(de)]) {
      expect(text).not.toBe("");
    }
  });
});
