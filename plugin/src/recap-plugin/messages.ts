// UI strings, bundled into the script so the build stays one file. English is the base language; the user's BBB
// locale picks German for "de" and its regions. The AI minutes are BBB's recording of the meeting: a paused one can be
// resumed in the same meeting, like with BBB's own record button.

export interface Messages {
  pillStart: string;
  pillRunning: string;
  pillPaused: string;
  tooltipOff: string;
  tooltipRunning: string;
  tooltipPaused: string;
  startTitle: string;
  startSpoken: string;
  startSpokenDetail: string;
  noVideo: string;
  startData: string;
  startNotice: string;
  pauseTitle: string;
  pauseText: string;
  startButton: string;
  pauseButton: string;
  cancel: string;
  chipRunning: string;
  chipPaused: string;
  chipTooltipRunning: string;
  chipTooltipPaused: string;
  infoTitle: string;
  infoSpoken: string;
  infoData: string;
  infoClose: string;
  toastStartedModeratorTitle: string;
  toastStartedModeratorBody: string;
  toastStartedParticipantTitle: string;
  toastStartedParticipantBody: string;
  toastPausedTitle: string;
  toastPausedBody: string;
}

const en: Messages = {
  pillStart: "Start AI minutes",
  pillRunning: "AI minutes",
  pillPaused: "AI minutes paused",
  tooltipOff: "Start AI minutes. They are off.",
  tooltipRunning: "AI minutes running. Click to pause.",
  tooltipPaused: "AI minutes paused. Click to resume.",
  startTitle: "Start AI minutes?",
  startSpoken: "Everything said is transcribed",
  startSpokenDetail: ", with names and timestamps.",
  noVideo: "No video in the minutes.",
  startData: "Including chat, participant list and meeting data.",
  startNotice: "Everyone sees a notice while minutes are being taken.",
  pauseTitle: "Pause AI minutes?",
  pauseText: "Nothing is transcribed until you resume.",
  startButton: "Start",
  pauseButton: "Pause",
  cancel: "Cancel",
  chipRunning: "Minutes are being taken",
  chipPaused: "Minutes paused",
  chipTooltipRunning: "Minutes are being taken. Click for details.",
  chipTooltipPaused: "Minutes paused. Click for details.",
  infoTitle: "This meeting is being minuted",
  infoSpoken: "What is said is transcribed with names, no video in the minutes",
  infoData: "Chat, participant list and meeting data are included",
  infoClose: "Got it",
  toastStartedModeratorTitle: "AI minutes are running",
  toastStartedModeratorBody: "All participants have been notified.",
  toastStartedParticipantTitle: "This meeting is now being minuted",
  toastStartedParticipantBody: "What is said is transcribed with names.",
  toastPausedTitle: "AI minutes paused",
  toastPausedBody: "Nothing is transcribed until they are resumed.",
};

const de: Messages = {
  pillStart: "KI-Protokoll starten",
  pillRunning: "KI-Protokoll",
  pillPaused: "KI-Protokoll pausiert",
  tooltipOff: "KI-Protokoll starten. Es ist aus.",
  tooltipRunning: "KI-Protokoll läuft. Klicken zum Pausieren.",
  tooltipPaused: "KI-Protokoll pausiert. Klicken zum Fortsetzen.",
  startTitle: "KI-Protokoll starten?",
  startSpoken: "Alles Gesagte wird transkribiert",
  startSpokenDetail: ", mit Namen und Zeitstempeln.",
  noVideo: "Kein Video im Protokoll.",
  startData: "Inkl. Chat, Teilnehmerliste und Meeting-Daten.",
  startNotice: "Alle sehen einen Hinweis, solange protokolliert wird.",
  pauseTitle: "KI-Protokoll pausieren?",
  pauseText: "Bis zum Fortsetzen wird nichts transkribiert.",
  startButton: "Starten",
  pauseButton: "Pausieren",
  cancel: "Abbrechen",
  chipRunning: "Wird protokolliert",
  chipPaused: "Protokoll pausiert",
  chipTooltipRunning: "Wird protokolliert. Klicken für Details.",
  chipTooltipPaused: "Protokoll pausiert. Klicken für Details.",
  infoTitle: "Dieses Meeting wird protokolliert",
  infoSpoken: "Gesagtes wird mit Namen transkribiert, kein Video im Protokoll",
  infoData: "Chat, Teilnehmerliste und Meeting-Daten fließen ein",
  infoClose: "Verstanden",
  toastStartedModeratorTitle: "KI-Protokoll läuft",
  toastStartedModeratorBody: "Alle Teilnehmenden wurden informiert.",
  toastStartedParticipantTitle: "Dieses Meeting wird jetzt protokolliert",
  toastStartedParticipantBody: "Gesagtes wird mit Namen transkribiert.",
  toastPausedTitle: "KI-Protokoll pausiert",
  toastPausedBody: "Bis zum Fortsetzen wird nichts transkribiert.",
};

const catalog: Record<string, Messages> = { en, de };

// Picks the strings for a BBB locale such as "de", "de-DE" or "pt_BR"; English for everything else.
export function messagesFor(locale: string | undefined): Messages {
  const language = (locale ?? "").split(/[-_]/)[0].toLowerCase();
  return catalog[language] ?? en;
}
