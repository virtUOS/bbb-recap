import { useEffect, useState } from "react";
import {
  BbbPluginSdk,
  IntlLocaleUiDataNames,
  NavBarButton,
  NavBarItemPosition,
  type PluginApi,
} from "bigbluebutton-html-plugin-sdk";

import { useClock, useReducedMotion, useServerOffset } from "./hooks";
import { messagesFor } from "./messages";
import { PausePopover, StartPopover } from "./minutes-popovers";
import { type PopoverKind, pillItem, popoverOnClick, popoverStillFits } from "./nav-items";
import { installPageStyle } from "./page-style";
import {
  CURRENT_TIME,
  type CurrentTimeData,
  MEETING_RECORDING,
  type MeetingRecordingData,
  MODERATOR_ROLE,
  RECORD_PERMISSION,
  RECORDING_POLICIES,
  type RecordingPoliciesData,
  type RecordPermissionData,
  SET_RECORDING_STATUS,
  activeSeconds,
  allLoaded,
  mayRecord,
  minutesState,
  parseRecordPermission,
  recordingPolicies,
} from "./recording";
import type { RecapPluginProps } from "./types";

// The "AI minutes" pill in the nav bar, right after BBB's recording indicator (bigbluebutton-html5/imports/ui/
// components/nav-bar/component.jsx:329-344 @ 7d27e7e). The AI minutes are the meeting's recording: the pill starts and
// pauses it with the mutation of BBB's own record button and shows the state of the recording, whoever changed it.
// It shows only to users who may record (recording.ts mayRecord).
export default function RecapPlugin({ pluginUuid }: RecapPluginProps) {
  BbbPluginSdk.initialize(pluginUuid);
  const pluginApi: PluginApi = BbbPluginSdk.getPluginApi(pluginUuid);

  const currentUser = pluginApi.useCurrentUser!();
  const policies = pluginApi.useCustomSubscription!<RecordingPoliciesData>(RECORDING_POLICIES);
  const recordingState = pluginApi.useCustomSubscription!<MeetingRecordingData>(MEETING_RECORDING);
  const permission = pluginApi.useCustomSubscription!<RecordPermissionData>(RECORD_PERMISSION);
  const currentTime = pluginApi.useCustomQuery!<CurrentTimeData>(CURRENT_TIME);
  const [setRecordingStatus] = pluginApi.useCustomMutation!<{ recording: boolean }>(SET_RECORDING_STATUS);
  const { locale } = pluginApi.useUiData!(IntlLocaleUiDataNames.CURRENT_LOCALE, { locale: "en", fallbackLocale: "en" });
  const reducedMotion = useReducedMotion();
  const now = useClock();
  const offset = useServerOffset(currentTime.data);

  // The open popover, or null.
  const [popover, setPopover] = useState<PopoverKind | null>(null);

  const messages = messagesFor(locale);
  const controller =
    allLoaded([currentUser, policies, recordingState, permission]) &&
    mayRecord({
      ...recordingPolicies(policies.data),
      isModerator: currentUser?.data?.role === MODERATOR_ROLE,
      recordPermission: parseRecordPermission(permission.data),
    });
  const state = minutesState(recordingState.data ?? undefined);
  // Changes once a second while running and not at all while off or paused, so the nav bar is updated only then.
  const seconds = activeSeconds(recordingState.data ?? undefined, now + offset);

  // A popover that no longer fits must not stay open or come back by itself. Adjusting state during render, as React
  // recommends instead of an effect (react.dev "You Might Not Need an Effect").
  if (popover !== null && !popoverStillFits(popover, controller, state)) {
    setPopover(null);
  }

  useEffect(installPageStyle, []);

  useEffect(() => {
    if (!controller) {
      pluginApi.setNavBarItems([]);
      return;
    }
    pluginApi.setNavBarItems([
      new NavBarButton({
        position: NavBarItemPosition.RIGHT,
        ...pillItem({
          state,
          seconds,
          messages,
          reducedMotion,
          // The mutation trigger is null until the client has set it up.
          disabled: !setRecordingStatus,
          onClick: () => {
            const kind = popoverOnClick(state);
            if (kind === null) {
              // Paused: resume at once, without a popover.
              setRecordingStatus?.({ variables: { recording: true } });
            } else {
              setPopover(kind);
            }
          },
        }),
      }),
    ]);
  }, [controller, state, seconds, messages, reducedMotion, setRecordingStatus, pluginApi]);

  useEffect(() => () => void pluginApi.setNavBarItems([]), [pluginApi]);

  // The server ignores a request for the state the meeting already has (SetRecordingStatusCmdMsgHdlr.scala:49-50), so
  // a confirmation that crosses another user's change does no harm.
  const setRecording = (recording: boolean) => setRecordingStatus?.({ variables: { recording } });
  const close = () => setPopover(null);
  switch (popover) {
    case "start":
      return <StartPopover messages={messages} onConfirm={() => setRecording(true)} onClose={close} />;
    case "pause":
      return <PausePopover messages={messages} onConfirm={() => setRecording(false)} onClose={close} />;
    default:
      return null;
  }
}
