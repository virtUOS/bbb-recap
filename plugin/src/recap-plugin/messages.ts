// UI strings, bundled into the script so the build stays one file. BBB "pauses" a recording: a stopped recording
// can be started again in the same meeting, like with its own record button.

export interface Messages {
  labelOff: string;
  labelOn: string;
  tooltipStart: string;
  tooltipPause: string;
  startTitle: string;
  startText: string;
  pauseTitle: string;
  pauseText: string;
  start: string;
  pause: string;
  cancel: string;
}

const en: Messages = {
  labelOff: "AI summary: off",
  labelOn: "AI summary: on",
  tooltipStart: "Start the AI summary. This starts the recording of the meeting.",
  tooltipPause: "Pause the AI summary. This pauses the recording of the meeting.",
  startTitle: "Start the AI summary?",
  startText:
    "This starts the recording of the meeting. Everyone in the meeting is notified. " +
    "After the meeting, the recorded parts are transcribed and summarized.",
  pauseTitle: "Pause the AI summary?",
  pauseText: "This pauses the recording of the meeting. You can start it again later.",
  start: "Start",
  pause: "Pause",
  cancel: "Cancel",
};

const de: Messages = {
  labelOff: "KI-Zusammenfassung: aus",
  labelOn: "KI-Zusammenfassung: an",
  tooltipStart: "KI-Zusammenfassung starten. Dadurch startet die Aufzeichnung der Konferenz.",
  tooltipPause: "KI-Zusammenfassung pausieren. Dadurch pausiert die Aufzeichnung der Konferenz.",
  startTitle: "KI-Zusammenfassung starten?",
  startText:
    "Dadurch startet die Aufzeichnung der Konferenz. Alle Teilnehmenden werden benachrichtigt. " +
    "Nach der Konferenz werden die aufgezeichneten Teile transkribiert und zusammengefasst.",
  pauseTitle: "KI-Zusammenfassung pausieren?",
  pauseText: "Dadurch pausiert die Aufzeichnung der Konferenz. Sie können sie später wieder starten.",
  start: "Starten",
  pause: "Pausieren",
  cancel: "Abbrechen",
};

const catalog: Record<string, Messages> = { en, de };

// Picks the strings for a BBB locale such as "de", "de-DE" or "pt_BR"; English for everything else.
export function messagesFor(locale: string | undefined): Messages {
  const language = (locale ?? "").split(/[-_]/)[0].toLowerCase();
  return catalog[language] ?? en;
}
