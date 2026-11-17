// Props of the nav bar items: the pill for users who may start and pause the AI minutes, the chip for everyone else.
// Pure, so the unit tests cover every state.
//
// BBB renders the label inside a <span> (common/button/component.jsx:295-305 @ 7d27e7e), so a node of plain elements
// works although the SDK types it as a string; it lets the timer have its own colour and tabular digits. The tooltip
// is always set: BBB uses it as the aria-label (nav-bar/component.jsx:85,91), so it carries the state for screen
// readers instead of aria-pressed, which the nav bar does not pass on. It has no timer, so readers are not flooded.
import type { CSSProperties, ReactNode } from "react";
import type { NavBarButton } from "bigbluebutton-html-plugin-sdk";

import { chipDot, infoIcon, pillIcon } from "./icons";
import type { Messages } from "./messages";
import { formatDuration, type MinutesState } from "./recording";

// Without the position, which is an SDK value: the SDK cannot be loaded outside a browser, and the tests run in Node.
export type NavBarButtonProps = Omit<ConstructorParameters<typeof NavBarButton>[0], "position">;

export const PILL_ID = "recap-ai-minutes";
export const PILL_DATA_TEST = "recapAiMinutesButton";

// BBB's own recording pill: 2.25rem high, fully rounded, the ghost background of its idle state
// (recording-indicator/styles.ts:108-109,141-144,301-305; stylesheets/styled-components/palette.js:61).
const IDLE_BG = "var(--btn-default-bg, rgba(255, 255, 255, 0.1))";
// Like BBB's recording-active background (palette.js:68), in violet.
const RUNNING_BG = "color-mix(in srgb, #A78BFA 22%, transparent)";
const WHITE = "var(--color-white, #FFF)";

const pillBase: CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: "0.15rem",
  boxSizing: "border-box",
  height: "2.25rem",
  padding: "0 1rem 0 0.625rem",
  borderRadius: "1.125rem",
  fontSize: "0.9375rem",
  fontWeight: 600,
  lineHeight: 1,
  whiteSpace: "nowrap",
};

const pillStyles: Record<MinutesState, CSSProperties> = {
  // A transparent border keeps the box the same size in every state.
  off: { ...pillBase, paddingLeft: "0.75rem", background: IDLE_BG, border: "1px solid transparent", color: WHITE },
  running: { ...pillBase, background: RUNNING_BG, border: "1px solid rgba(167, 139, 250, 0.55)", color: "#F1EDFF" },
  paused: { ...pillBase, background: IDLE_BG, border: "1px dashed rgba(167, 139, 250, 0.6)", color: WHITE },
};

function withTimer(text: string, seconds: number, color: string): ReactNode {
  return (
    <span>
      {text}
      <span style={{ marginLeft: "0.35em", color, fontWeight: 400, fontVariantNumeric: "tabular-nums" }}>
        {`· ${formatDuration(seconds)}`}
      </span>
    </span>
  );
}

export interface PillInput {
  state: MinutesState;
  seconds: number;
  messages: Messages;
  reducedMotion: boolean;
  disabled: boolean;
  onClick: () => void;
}

export function pillItem({ state, seconds, messages, reducedMotion, disabled, onClick }: PillInput): NavBarButtonProps {
  const label: Record<MinutesState, ReactNode> = {
    off: messages.pillStart,
    running: withTimer(messages.pillRunning, seconds, "#C9BEF8"),
    paused: withTimer(messages.pillPaused, seconds, "#A7B0BC"),
  };
  const tooltip: Record<MinutesState, string> = {
    off: messages.tooltipOff,
    running: messages.tooltipRunning,
    paused: messages.tooltipPaused,
  };
  return {
    // A fixed id keeps the button mounted across the updates of the timer; without one, BBB assigns a new id on
    // every call (plugins-engine/extensible-areas/manager.tsx:47-54) and the button loses focus every second.
    id: PILL_ID,
    label: label[state] as unknown as string,
    tooltip: tooltip[state],
    icon: { svgContent: pillIcon(state, reducedMotion) },
    // "default" has no !important rules, so the inline style applies in full (common/button/styles.js:893-955).
    color: "default",
    style: pillStyles[state],
    disabled,
    hasSeparator: false,
    dataTest: PILL_DATA_TEST,
    onClick,
  };
}

export const CHIP_ID = "recap-ai-minutes-chip";
export const CHIP_DATA_TEST = "recapAiMinutesChip";

const chipStyle: CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: "0.1rem",
  boxSizing: "border-box",
  height: "2rem",
  padding: "0 0.75rem 0 0.5rem",
  borderRadius: "1rem",
  border: "1px solid rgba(167, 139, 250, 0.45)",
  background: "transparent",
  color: "#DDD6FE",
  fontSize: "0.875rem",
  fontWeight: 600,
  lineHeight: 1,
  whiteSpace: "nowrap",
};

export interface ChipInput {
  state: "running" | "paused";
  messages: Messages;
  reducedMotion: boolean;
  onClick: () => void;
}

// The status chip for everyone without the control. The whole chip is the button; the info icon is decoration.
export function chipItem({ state, messages, reducedMotion, onClick }: ChipInput): NavBarButtonProps {
  const text = state === "running" ? messages.chipRunning : messages.chipPaused;
  const label = (
    <span style={{ display: "inline-flex", alignItems: "center", gap: "0.45rem" }}>
      {text}
      {infoIcon()}
    </span>
  );
  return {
    id: CHIP_ID,
    label: label as unknown as string,
    tooltip: state === "running" ? messages.chipTooltipRunning : messages.chipTooltipPaused,
    icon: { svgContent: chipDot(state, reducedMotion) },
    color: "default",
    style: chipStyle,
    disabled: false,
    hasSeparator: false,
    dataTest: CHIP_DATA_TEST,
    onClick,
  };
}

// What the nav bar shows: the pill to users who may start and pause, the chip to everyone else once the minutes
// have been started, nothing while the data is loading.
export type NavView = "pill" | "chip" | null;

export function navView(loaded: boolean, controller: boolean, state: MinutesState): NavView {
  if (!loaded) {
    return null;
  }
  if (controller) {
    return "pill";
  }
  return state === "off" ? null : "chip";
}

export type PopoverKind = "start" | "pause" | "info";

// The popover a click on the pill opens; null for paused, which resumes at once.
export function popoverOnClick(state: MinutesState): PopoverKind | null {
  return state === "off" ? "start" : state === "running" ? "pause" : null;
}

// An open popover closes by itself once it no longer fits: the user lost the control, the chip went away, or the
// state changed under it (another moderator, or BBB's own record button).
export function popoverStillFits(kind: PopoverKind, view: NavView, state: MinutesState): boolean {
  if (kind === "info") {
    return view === "chip";
  }
  return view === "pill" && popoverOnClick(state) === kind;
}
