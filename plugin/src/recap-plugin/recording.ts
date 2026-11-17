// Recording state, timer and permission rules of the AI minutes. No React and no SDK imports, so the unit tests
// run without a BBB client. Sources: bigbluebutton/bigbluebutton @ 7d27e7e (v4.0.x).

// The mutation of BBB's own record button
// (bigbluebutton-html5/imports/ui/components/recording/mutations.jsx:3-9).
export const SET_RECORDING_STATUS = `mutation SetRecordingStatus($recording: Boolean!) {
  meetingRecordingSetStatus(recording: $recording)
}`;

// The data BBB's record button reads
// (bigbluebutton-html5/imports/ui/components/nav-bar/nav-bar-graphql/recording-indicator/queries.ts:32-41,
// bbb-graphql-server/metadata/databases/BigBlueButton/tables/public_v_meeting_recordingPolicies.yaml).
// One root field per subscription.
export const RECORDING_POLICIES = `subscription RecapRecordingPolicies {
  meeting {
    recordingPolicies {
      record
      allowStartStopRecording
    }
  }
}`;

// The fields of BBB's recording indicator (recording-indicator/queries.ts:32-41). The view holds the latest recording
// segment of the meeting; previousRecordedTimeInSeconds sums the finished segments
// (bbb-graphql-server/bbb_schema.sql:82-133).
export const MEETING_RECORDING = `subscription RecapMeetingRecording {
  meeting_recording {
    isRecording
    startedAt
    previousRecordedTimeInSeconds
  }
}`;

// The server clock, as BBB reads it for its own recording timer (bigbluebutton-html5/imports/ui/core/utils/
// timeSync.ts:5-20, bbb-graphql-server/bbb_schema.sql:2327-2330). BBB runs a plugin's query cache-first, and BBB's
// own timer writes current_time { currentTimestamp } to the cache (core/graphql/queries/timer.ts:38-44); the extra
// field currentTimeMillis, which that query does not select, makes the cache unable to answer, so the time is fresh.
export const CURRENT_TIME = `query RecapCurrentTime {
  current_time {
    currentTimestamp
    currentTimeMillis
  }
}`;

// The own user's join parameter userdata-bbb_record_permission
// (bbb-graphql-server/metadata/databases/BigBlueButton/tables/public_v_user_metadata.yaml: own user only).
export const RECORD_PERMISSION = `subscription RecapRecordPermission {
  user_metadata(where: { parameter: { _eq: "bbb_record_permission" } }) {
    value
  }
}`;

export interface RecordingPoliciesData {
  meeting?: { recordingPolicies?: { record?: boolean; allowStartStopRecording?: boolean } | null }[];
}

export interface MeetingRecordingData {
  meeting_recording?: {
    isRecording?: boolean;
    startedAt?: string | null;
    previousRecordedTimeInSeconds?: number | null;
  }[];
}

export interface CurrentTimeData {
  current_time?: { currentTimestamp?: string | null }[];
}

export interface RecordPermissionData {
  user_metadata?: { value?: string | null }[];
}

// Reads bbb_record_permission like the server does: Scala's String.toBoolean, so "true" and "false" in any case
// (akka-bbb-apps/src/main/scala/org/bigbluebutton/core/apps/users/SetRecordingStatusCmdMsgHdlr.scala:33-41).
// Any other value would make the server's check fail, so it counts as no permission. undefined means "not set".
export function parseRecordPermission(data: RecordPermissionData | undefined): boolean | undefined {
  const value = data?.user_metadata?.[0]?.value;
  if (value === undefined || value === null) {
    return undefined;
  }
  return value.toLowerCase() === "true";
}

export interface RecordRule {
  record: boolean;
  allowStartStopRecording: boolean;
  isModerator: boolean;
  recordPermission: boolean | undefined;
}

// Mirrors BBB's mayIRecord
// (bigbluebutton-html5/imports/ui/components/nav-bar/nav-bar-graphql/recording-indicator/service.tsx:3-10)
// plus the record flag, without which BBB hides its record button (recording-indicator/component.tsx:385).
// The server applies the same rule and ejects a user without permission
// (SetRecordingStatusCmdMsgHdlr.scala:33-46), so the button must never show to such a user.
export function mayRecord({ record, allowStartStopRecording, isModerator, recordPermission }: RecordRule): boolean {
  if (!record || !allowStartStopRecording) {
    return false;
  }
  if (recordPermission !== undefined) {
    return recordPermission;
  }
  return isModerator;
}

// Role value of a moderator in the SDK's useCurrentUser
// (akka-bbb-apps/src/main/scala/org/bigbluebutton/core/models/Users2x.scala:511).
export const MODERATOR_ROLE = "MODERATOR";

// The state of the AI minutes. They are the meeting's recording: off until the first start, then running or paused.
// A segment row exists only after a start (bbb_schema.sql:82-90, startedAt is part of the primary key).
export type MinutesState = "off" | "running" | "paused";

export function minutesState(data: MeetingRecordingData | undefined): MinutesState {
  const row = data?.meeting_recording?.[0];
  if (row?.isRecording === true) {
    return "running";
  }
  return row?.startedAt ? "paused" : "off";
}

// Seconds of active (not paused) time, the same for every client: the finished segments plus, while running, the
// time since the current segment started, measured on the server clock. This is the formula of BBB's own timer
// (recording-indicator/component.tsx:488-493), so both show the same time.
export function activeSeconds(data: MeetingRecordingData | undefined, serverNowMs: number): number {
  const row = data?.meeting_recording?.[0];
  const previous = Math.max(0, row?.previousRecordedTimeInSeconds ?? 0);
  if (row?.isRecording !== true || !row.startedAt) {
    return Math.floor(previous);
  }
  const startedAt = new Date(row.startedAt).getTime();
  if (Number.isNaN(startedAt)) {
    return Math.floor(previous);
  }
  return Math.floor(previous + Math.max(0, (serverNowMs - startedAt) / 1000));
}

// Server time minus client time in milliseconds, from a current_time answer received at receivedAtMs for a request
// sent at sentAtMs; half the round trip is taken as the delay, like BBB (timeSync.ts:5-20). 0 when unknown.
export function serverOffset(data: CurrentTimeData | undefined | null, sentAtMs: number, receivedAtMs: number): number {
  const stamp = data?.current_time?.[0]?.currentTimestamp;
  const serverMs = stamp ? new Date(stamp).getTime() : NaN;
  if (Number.isNaN(serverMs)) {
    return 0;
  }
  return serverMs - (sentAtMs + (receivedAtMs - sentAtMs) / 2);
}

// mm:ss, or h:mm:ss from one hour on.
export function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}

export function recordingPolicies(data: RecordingPoliciesData | undefined): {
  record: boolean;
  allowStartStopRecording: boolean;
} {
  const policies = data?.meeting?.[0]?.recordingPolicies;
  return {
    record: policies?.record === true,
    allowStartStopRecording: policies?.allowStartStopRecording === true,
  };
}

// The plugin loads in every meeting created with meta_recap-enabled, whatever its value: bbb-web only checks that
// the placeholder in manifest.json can be filled. The value is passed on in the script URL, and only the literal
// "true" opts in (docs/reference/metadata-interface.md §1.2). bbb-web inserts the value unescaped
// (bbb-common-web/.../api/util/PluginUtils.java:257-269), so "true&x=1" or "true#x" would also read as "true"; the
// URL must therefore hold recap-enabled=true and at most BBB's own version parameter
// (akka-bbb-apps/.../core/models/Plugins.scala:128-140), nothing else.
export function isRecapEnabled(scriptUrl: string | null | undefined): boolean {
  if (!scriptUrl) {
    return false;
  }
  let url: URL;
  try {
    url = new URL(scriptUrl, "https://invalid.invalid/");
  } catch {
    return false;
  }
  if (url.hash !== "") {
    return false;
  }
  const keys = [...url.searchParams.keys()];
  const count = (key: string) => keys.filter((k) => k === key).length;
  return (
    url.searchParams.get("recap-enabled") === "true" &&
    count("recap-enabled") === 1 &&
    count("version") <= 1 &&
    keys.length === 1 + count("version")
  );
}

export interface Response<T> {
  loading?: boolean;
  error?: unknown;
  data?: T | null;
}

// True only when every source has finished loading without an error and has data. Until then the button stays
// hidden: deciding on missing data could show it to a user without permission, whom the server would eject.
export function allLoaded(responses: (Response<unknown> | undefined)[]): boolean {
  return responses.every((r) => r !== undefined && !r.loading && !r.error && r.data !== undefined && r.data !== null);
}
