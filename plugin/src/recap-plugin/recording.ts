// Recording state and permission rules of the AI summary button. No React and no SDK imports, so the unit tests
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

export const MEETING_RECORDING = `subscription RecapMeetingRecording {
  meeting_recording {
    isRecording
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
  meeting_recording?: { isRecording?: boolean }[];
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

export function isRecording(data: MeetingRecordingData | undefined): boolean {
  return data?.meeting_recording?.[0]?.isRecording === true;
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
// "true" opts in (docs/reference/metadata-interface.md §1.2).
export function isRecapEnabled(scriptUrl: string | null | undefined): boolean {
  if (!scriptUrl) {
    return false;
  }
  try {
    return new URL(scriptUrl, "https://invalid.invalid/").searchParams.get("recap-enabled") === "true";
  } catch {
    return false;
  }
}
