// Which toast a change of the AI minutes shows to whom. Every client watches the meeting's recording state and
// notifies on a change only, not for the state it finds when it joins.
import type { Messages } from "./messages";
import type { MinutesState } from "./recording";

export type ToastKind = "startedModerator" | "startedParticipant" | "paused";

// controller: the user may start and pause. They get "running" only for the first start: whoever resumes has just
// clicked the pill. Everyone else gets a toast whenever transcription starts again, so they are told actively, not
// only through the chip. A pause goes to everyone.
export function toastFor(
  previous: MinutesState | undefined,
  next: MinutesState,
  controller: boolean,
): ToastKind | null {
  if (previous === undefined || previous === next) {
    return null;
  }
  if (next === "running") {
    if (!controller) {
      return "startedParticipant";
    }
    return previous === "off" ? "startedModerator" : null;
  }
  if (next === "paused" && previous === "running") {
    return "paused";
  }
  return null;
}

export function toastText(kind: ToastKind, messages: Messages): { title: string; body: string } {
  switch (kind) {
    case "startedModerator":
      return { title: messages.toastStartedModeratorTitle, body: messages.toastStartedModeratorBody };
    case "startedParticipant":
      return { title: messages.toastStartedParticipantTitle, body: messages.toastStartedParticipantBody };
    case "paused":
      return { title: messages.toastPausedTitle, body: messages.toastPausedBody };
  }
}
