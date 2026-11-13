import { describe, expect, it } from "vitest";

import {
  allLoaded,
  isRecapEnabled,
  isRecording,
  mayRecord,
  parseRecordPermission,
  recordingPolicies,
} from "./recording";

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

describe("recording state", () => {
  it("reads isRecording", () => {
    expect(isRecording({ meeting_recording: [{ isRecording: true }] })).toBe(true);
    expect(isRecording({ meeting_recording: [{ isRecording: false }] })).toBe(false);
    expect(isRecording({ meeting_recording: [] })).toBe(false);
    expect(isRecording(undefined)).toBe(false);
  });

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
