import { useEffect, useRef, useState } from "react";
import {
  BbbPluginSdk,
  IntlLocaleUiDataNames,
  NavBarButton,
  NavBarItemPosition,
  NotificationTypeUiCommand,
  type PluginApi,
} from "bigbluebutton-html-plugin-sdk";

import { useClock, useReducedMotion, useServerOffset } from "./hooks";
import { messagesFor } from "./messages";
import { toastIcon } from "./icons";
import { InfoPopover, PausePopover, StartPopover } from "./minutes-popovers";
import { type PopoverKind, chipItem, navView, pillItem, popoverOnClick, popoverStillFits } from "./nav-items";
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
  type MinutesState,
  minutesState,
  parseRecordPermission,
  recordingPolicies,
} from "./recording";
import { toastFor, toastText } from "./toasts";
import type { RecapPluginProps } from "./types";

// The "AI minutes" pill in the nav bar, right after BBB's recording indicator (bigbluebutton-html5/imports/ui/
// components/nav-bar/component.jsx:329-344 @ 7d27e7e). The AI minutes are the meeting's recording: the pill starts and
// pauses it with the mutation of BBB's own record button and shows the state of the recording, whoever changed it.
// Users who may record (recording.ts mayRecord) get the pill; everyone else gets a status chip once the minutes have
// been started, and a toast on each change.
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
  const loaded = allLoaded([currentUser, policies, recordingState, permission]);
  const controller =
    loaded &&
    mayRecord({
      ...recordingPolicies(policies.data),
      isModerator: currentUser?.data?.role === MODERATOR_ROLE,
      recordPermission: parseRecordPermission(permission.data),
    });
  const state = minutesState(recordingState.data ?? undefined);
  const view = navView(loaded, controller, state);
  // Changes once a second while running and not at all while off or paused, so the nav bar is updated only then.
  const seconds = activeSeconds(recordingState.data ?? undefined, now + offset);

  // A popover that no longer fits must not stay open or come back by itself. Adjusting state during render, as React
  // recommends instead of an effect (react.dev "You Might Not Need an Effect").
  if (popover !== null && !popoverStillFits(popover, view, state)) {
    setPopover(null);
  }

  useEffect(installPageStyle, []);

  // The chip has no timer, so only the pill follows the seconds.
  const shownSeconds = view === "pill" ? seconds : 0;
  useEffect(() => {
    let item;
    if (view === "pill") {
      item = pillItem({
        state,
        seconds: shownSeconds,
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
      });
    } else if (view === "chip" && state !== "off") {
      item = chipItem({ state, messages, reducedMotion, onClick: () => setPopover("info") });
    } else {
      pluginApi.setNavBarItems([]);
      return;
    }
    pluginApi.setNavBarItems([new NavBarButton({ position: NavBarItemPosition.RIGHT, ...item })]);
  }, [view, state, shownSeconds, messages, reducedMotion, setRecordingStatus, pluginApi]);

  useEffect(() => () => void pluginApi.setNavBarItems([]), [pluginApi]);

  // Toasts on a change of the state, in every client, by role. The first state after loading is not a change.
  const previous = useRef<MinutesState | undefined>(undefined);
  useEffect(() => {
    if (!loaded) {
      return;
    }
    const kind = toastFor(previous.current, state, controller);
    previous.current = state;
    if (kind !== null) {
      const { title, body } = toastText(kind, messages);
      pluginApi.uiCommands?.notification.send({
        message: title,
        content: body,
        icon: { svgContent: toastIcon() },
        type: NotificationTypeUiCommand.DEFAULT,
      });
    }
  }, [loaded, state, controller, messages, pluginApi]);

  // The server ignores a request for the state the meeting already has (SetRecordingStatusCmdMsgHdlr.scala:49-50), so
  // a confirmation that crosses another user's change does no harm.
  const setRecording = (recording: boolean) => setRecordingStatus?.({ variables: { recording } });
  const close = () => setPopover(null);
  switch (popover) {
    case "start":
      return <StartPopover messages={messages} onConfirm={() => setRecording(true)} onClose={close} />;
    case "pause":
      return <PausePopover messages={messages} onConfirm={() => setRecording(false)} onClose={close} />;
    case "info":
      return <InfoPopover messages={messages} onClose={close} />;
    default:
      return null;
  }
}
