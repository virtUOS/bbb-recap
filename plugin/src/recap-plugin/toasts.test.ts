import { describe, expect, it } from "vitest";

import { messagesFor } from "./messages";
import { toastFor, toastText } from "./toasts";

describe("toastFor", () => {
  it("shows nothing for the state found on joining or an unchanged state", () => {
    for (const state of ["off", "running", "paused"] as const) {
      expect(toastFor(undefined, state, true)).toBeNull();
      expect(toastFor(undefined, state, false)).toBeNull();
      expect(toastFor(state, state, true)).toBeNull();
      expect(toastFor(state, state, false)).toBeNull();
    }
  });

  it("start: the moderator toast to users with the control, the participant toast to everyone else", () => {
    expect(toastFor("off", "running", true)).toBe("startedModerator");
    expect(toastFor("off", "running", false)).toBe("startedParticipant");
  });

  it("resume: only participants are told again", () => {
    expect(toastFor("paused", "running", true)).toBeNull();
    expect(toastFor("paused", "running", false)).toBe("startedParticipant");
  });

  it("pause: everyone", () => {
    expect(toastFor("running", "paused", true)).toBe("paused");
    expect(toastFor("running", "paused", false)).toBe("paused");
  });
});

describe("toastText", () => {
  it("names no destination of the minutes", () => {
    for (const locale of ["en", "de"]) {
      for (const kind of ["startedModerator", "startedParticipant", "paused"] as const) {
        const { title, body } = toastText(kind, messagesFor(locale));
        expect(`${title} ${body}`).not.toMatch(/mail|send|sent|versend|geht .* an/i);
        expect(title).not.toBe("");
        expect(body).not.toBe("");
      }
    }
  });

  it("uses the locale", () => {
    expect(toastText("startedParticipant", messagesFor("de")).title).toBe("Dieses Meeting wird jetzt protokolliert");
    expect(toastText("paused", messagesFor("en")).title).toBe("AI minutes paused");
  });
});
