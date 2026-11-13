import { useEffect, useState } from "react";
import type { SVGProps } from "react";
import {
  BbbPluginSdk,
  IntlLocaleUiDataNames,
  NavBarButton,
  NavBarItemPosition,
  type PluginApi,
} from "bigbluebutton-html-plugin-sdk";

import ConfirmDialog from "./confirm-dialog";
import { messagesFor } from "./messages";
import {
  MEETING_RECORDING,
  type MeetingRecordingData,
  MODERATOR_ROLE,
  RECORD_PERMISSION,
  RECORDING_POLICIES,
  type RecordingPoliciesData,
  type RecordPermissionData,
  SET_RECORDING_STATUS,
  allLoaded,
  isRecording,
  mayRecord,
  parseRecordPermission,
  recordingPolicies,
} from "./recording";
import type { RecapPluginProps } from "./types";

function recordIcon(on: boolean) {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" aria-hidden="true">
      <circle cx="12" cy="12" r="7" fill={on ? "currentColor" : "none"} stroke="currentColor" strokeWidth="2" />
    </svg>
  ) as SVGProps<SVGSVGElement>;
}

// A nav bar button right of BBB's recording indicator (bigbluebutton-html5/imports/ui/components/nav-bar/
// component.jsx:329-335 @ 7d27e7e). It starts and pauses the meeting recording with the mutation of BBB's own record
// button, after a confirmation. It shows only to users who may record (recording.ts mayRecord).
export default function RecapPlugin({ pluginUuid }: RecapPluginProps) {
  BbbPluginSdk.initialize(pluginUuid);
  const pluginApi: PluginApi = BbbPluginSdk.getPluginApi(pluginUuid);

  const currentUser = pluginApi.useCurrentUser!();
  const policies = pluginApi.useCustomSubscription!<RecordingPoliciesData>(RECORDING_POLICIES);
  const recordingState = pluginApi.useCustomSubscription!<MeetingRecordingData>(MEETING_RECORDING);
  const permission = pluginApi.useCustomSubscription!<RecordPermissionData>(RECORD_PERMISSION);
  const [setRecordingStatus] = pluginApi.useCustomMutation!<{ recording: boolean }>(SET_RECORDING_STATUS);
  const { locale } = pluginApi.useUiData!(IntlLocaleUiDataNames.CURRENT_LOCALE, { locale: "en", fallbackLocale: "en" });

  // The recording state to ask for in the open confirmation, or null when no confirmation is open.
  const [pending, setPending] = useState<boolean | null>(null);

  const messages = messagesFor(locale);
  const allowed =
    allLoaded([currentUser, policies, recordingState, permission]) &&
    mayRecord({
      ...recordingPolicies(policies.data),
      isModerator: currentUser?.data?.role === MODERATOR_ROLE,
      recordPermission: parseRecordPermission(permission.data),
    });
  const recording = isRecording(recordingState.data);

  // A confirmation that was open when the user lost the permission must not come back by itself. Adjusting state
  // during render, as React recommends instead of an effect (react.dev "You Might Not Need an Effect").
  if (!allowed && pending !== null) {
    setPending(null);
  }

  useEffect(() => {
    if (!allowed) {
      pluginApi.setNavBarItems([]);
      return;
    }
    pluginApi.setNavBarItems([
      new NavBarButton({
        label: recording ? messages.labelOn : messages.labelOff,
        tooltip: recording ? messages.tooltipPause : messages.tooltipStart,
        icon: { svgContent: recordIcon(recording) },
        color: recording ? "danger" : "primary",
        // The mutation trigger is null until the client has set it up.
        disabled: !setRecordingStatus,
        position: NavBarItemPosition.RIGHT,
        hasSeparator: false,
        dataTest: "recapAiSummaryButton",
        onClick: () => setPending(!recording),
      }),
    ]);
  }, [allowed, recording, messages, setRecordingStatus, pluginApi]);

  useEffect(() => () => void pluginApi.setNavBarItems([]), [pluginApi]);

  if (pending === null || !allowed) {
    return null;
  }
  return (
    <ConfirmDialog
      title={pending ? messages.startTitle : messages.pauseTitle}
      text={pending ? messages.startText : messages.pauseText}
      confirmLabel={pending ? messages.start : messages.pause}
      cancelLabel={messages.cancel}
      // The server ignores a request for the state the meeting already has
      // (SetRecordingStatusCmdMsgHdlr.scala:49-50), so a stale confirmation does no harm.
      onConfirm={() => setRecordingStatus?.({ variables: { recording: pending } })}
      onClose={() => setPending(null)}
    />
  );
}
