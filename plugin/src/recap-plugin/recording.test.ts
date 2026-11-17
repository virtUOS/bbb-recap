import { describe, expect, it } from "vitest";

import {
  activeSeconds,
  allLoaded,
  formatDuration,
  isRecapEnabled,
  mayRecord,
  minutesState,
  parseRecordPermission,
  recordingPolicies,
  serverOffset,
} from "./recording";

// Hasura's timestamp format, microseconds and offset.
const STARTED = "2026-11-17T10:00:00.000000+00:00";

describe("mayRecord", () => {
  const base = { record: true, allowStartStopRecording: true, isModerator: false, recordPermission: undefined };

  it("allows a moderator without bbb_record_permission", () => {
    expect(mayRecord({ ...base, isModerator: true })).toBe(true);
  });

  it("denies a viewer without bbb_record_permission", () => {
    expect(mayRecord(base)).toBe(false);
  });

  it("lets bbb_record_permission override the role", () => {
    expect(mayRecord({ ...base, recordPermission: true })).toBe(true);
    expect(mayRecord({ ...base, isModerator: true, recordPermission: false })).toBe(false);
  });

  it("denies everyone when the meeting does not allow start and stop", () => {
    expect(mayRecord({ ...base, isModerator: true, allowStartStopRecording: false })).toBe(false);
    expect(mayRecord({ ...base, recordPermission: true, allowStartStopRecording: false })).toBe(false);
  });

  it("denies everyone when the meeting is not recorded", () => {
    expect(mayRecord({ ...base, isModerator: true, record: false })).toBe(false);
  });
});

describe("parseRecordPermission", () => {
  it("is undefined when the parameter is not set", () => {
    expect(parseRecordPermission(undefined)).toBeUndefined();
    expect(parseRecordPermission({ user_metadata: [] })).toBeUndefined();
    expect(parseRecordPermission({ user_metadata: [{ value: null }] })).toBeUndefined();
  });

  it("reads true and false in any case, like the server", () => {
    expect(parseRecordPermission({ user_metadata: [{ value: "true" }] })).toBe(true);
    expect(parseRecordPermission({ user_metadata: [{ value: "TRUE" }] })).toBe(true);
    expect(parseRecordPermission({ user_metadata: [{ value: "false" }] })).toBe(false);
  });

  it("treats any other value as no permission", () => {
    expect(parseRecordPermission({ user_metadata: [{ value: "yes" }] })).toBe(false);
    expect(parseRecordPermission({ user_metadata: [{ value: " true" }] })).toBe(false);
    expect(parseRecordPermission({ user_metadata: [{ value: "" }] })).toBe(false);
  });
});

describe("minutesState", () => {
  it("is off before the first start", () => {
    expect(minutesState({ meeting_recording: [] })).toBe("off");
    expect(minutesState(undefined)).toBe("off");
    expect(minutesState({ meeting_recording: [{ isRecording: false, startedAt: null }] })).toBe("off");
  });

  it("is running while recording", () => {
    expect(minutesState({ meeting_recording: [{ isRecording: true, startedAt: STARTED }] })).toBe("running");
  });

  it("is paused after a start", () => {
    expect(minutesState({ meeting_recording: [{ isRecording: false, startedAt: STARTED }] })).toBe("paused");
  });
});

describe("activeSeconds", () => {
  const startedMs = Date.parse(STARTED);

  it("is 0 before the first start", () => {
    expect(activeSeconds({ meeting_recording: [] }, startedMs)).toBe(0);
    expect(activeSeconds(undefined, startedMs)).toBe(0);
  });

  it("counts the running segment on the server clock", () => {
    const data = { meeting_recording: [{ isRecording: true, startedAt: STARTED, previousRecordedTimeInSeconds: 0 }] };
    expect(activeSeconds(data, startedMs + 65_400)).toBe(65);
  });

  it("adds the finished segments after a resume", () => {
    const data = { meeting_recording: [{ isRecording: true, startedAt: STARTED, previousRecordedTimeInSeconds: 120 }] };
    expect(activeSeconds(data, startedMs + 30_000)).toBe(150);
  });

  it("stands still while paused", () => {
    const data = {
      meeting_recording: [{ isRecording: false, startedAt: STARTED, previousRecordedTimeInSeconds: 724 }],
    };
    expect(activeSeconds(data, startedMs + 999_000)).toBe(724);
  });

  it("never counts time before the start, so a client clock behind the server gives no negative time", () => {
    const data = { meeting_recording: [{ isRecording: true, startedAt: STARTED, previousRecordedTimeInSeconds: 10 }] };
    expect(activeSeconds(data, startedMs - 5_000)).toBe(10);
  });

  it("falls back to the finished segments for an unreadable start", () => {
    const data = { meeting_recording: [{ isRecording: true, startedAt: "x", previousRecordedTimeInSeconds: 7 }] };
    expect(activeSeconds(data, startedMs)).toBe(7);
  });

  it("is the same in every client once each has its server offset", () => {
    const data = { meeting_recording: [{ isRecording: true, startedAt: STARTED, previousRecordedTimeInSeconds: 0 }] };
    const serverNow = startedMs + 42_000;
    const fastClient = serverNow + 90_000;
    const slowClient = serverNow - 30_000;
    expect(activeSeconds(data, fastClient + (serverNow - fastClient))).toBe(42);
    expect(activeSeconds(data, slowClient + (serverNow - slowClient))).toBe(42);
  });
});

describe("serverOffset", () => {
  it("takes half the round trip as the delay", () => {
    const server = { current_time: [{ currentTimestamp: "2026-11-17T10:00:10.000Z" }] };
    // Sent at 10:00:00.000 client time, answered 200 ms later: server time 10:00:10.000 was at client 10:00:00.100.
    const sent = Date.parse("2026-11-17T10:00:00.000Z");
    expect(serverOffset(server, sent, sent + 200)).toBe(9_900);
  });

  it("is 0 without a usable answer", () => {
    expect(serverOffset(undefined, 0, 0)).toBe(0);
    expect(serverOffset({ current_time: [] }, 0, 0)).toBe(0);
    expect(serverOffset({ current_time: [{ currentTimestamp: "x" }] }, 0, 0)).toBe(0);
  });
});

describe("formatDuration", () => {
  it("shows mm:ss below one hour and h:mm:ss above", () => {
    expect(formatDuration(0)).toBe("00:00");
    expect(formatDuration(724)).toBe("12:04");
    expect(formatDuration(3599)).toBe("59:59");
    expect(formatDuration(3600)).toBe("1:00:00");
    expect(formatDuration(-3)).toBe("00:00");
  });
});

describe("recording policies", () => {
  it("reads the recording policies, false when missing", () => {
    expect(
      recordingPolicies({ meeting: [{ recordingPolicies: { record: true, allowStartStopRecording: true } }] }),
    ).toEqual({
      record: true,
      allowStartStopRecording: true,
    });
    expect(recordingPolicies({ meeting: [{ recordingPolicies: null }] })).toEqual({
      record: false,
      allowStartStopRecording: false,
    });
    expect(recordingPolicies(undefined)).toEqual({ record: false, allowStartStopRecording: false });
  });
});

describe("isRecapEnabled", () => {
  it("accepts only the literal true", () => {
    expect(
      isRecapEnabled("https://bbb.example.org/plugins/recap/RecapPlugin.js?recap-enabled=true&version=0.1.0"),
    ).toBe(true);
    expect(isRecapEnabled("https://bbb.example.org/plugins/recap/RecapPlugin.js?recap-enabled=false")).toBe(false);
    expect(isRecapEnabled("https://bbb.example.org/plugins/recap/RecapPlugin.js?recap-enabled=TRUE")).toBe(false);
    expect(isRecapEnabled("https://bbb.example.org/plugins/recap/RecapPlugin.js?recap-enabled=")).toBe(false);
  });

  it("rejects extra parameters or a fragment smuggled in with the value", () => {
    const base = "https://bbb.example.org/plugins/recap/RecapPlugin.js";
    expect(isRecapEnabled(`${base}?recap-enabled=true`)).toBe(true);
    expect(isRecapEnabled(`${base}?recap-enabled=true&x=1&version=0.1.0`)).toBe(false);
    expect(isRecapEnabled(`${base}?recap-enabled=true#x&version=0.1.0`)).toBe(false);
    expect(isRecapEnabled(`${base}?recap-enabled=true&version=0.1.0#x`)).toBe(false);
    expect(isRecapEnabled(`${base}?recap-enabled=true&recap-enabled=true`)).toBe(false);
    expect(isRecapEnabled(`${base}?recap-enabled=true&version=1&version=2`)).toBe(false);
  });

  it("is false without the parameter or a URL", () => {
    expect(isRecapEnabled("https://bbb.example.org/plugins/recap/RecapPlugin.js")).toBe(false);
    expect(isRecapEnabled(null)).toBe(false);
    expect(isRecapEnabled(undefined)).toBe(false);
  });
});

describe("allLoaded", () => {
  const ok = { loading: false, data: {} };

  it("is true when every source has data", () => {
    expect(allLoaded([ok, ok, { loading: false, data: { user_metadata: [] } }])).toBe(true);
  });

  it("is false while a source is loading", () => {
    expect(allLoaded([ok, { loading: true, data: {} }])).toBe(false);
    expect(allLoaded([ok, { loading: true }])).toBe(false);
  });

  it("is false when a source has an error", () => {
    expect(allLoaded([ok, { loading: false, error: new Error("x"), data: {} }])).toBe(false);
  });

  it("is false when a source has finished without data", () => {
    expect(allLoaded([ok, { loading: false }])).toBe(false);
    expect(allLoaded([ok, { loading: false, data: null }])).toBe(false);
  });

  it("is false when a source is missing", () => {
    expect(allLoaded([ok, undefined])).toBe(false);
  });
});
