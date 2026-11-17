// SVG icons. The nav bar and toast icons are rendered by BBB's React, so they are plain SVG elements only, no
// components or hooks (the plugin bundles its own React copy). BBB forces nav bar icons to 1em
// (plugins/plugin-icon/styles.tsx:10-13 @ 7d27e7e); an inline width and height override that. That rule matches every
// nested <svg> too, so inner parts are placed with <g transform>, which no CSS size touches.
import type { SVGProps } from "react";

import type { MinutesState } from "./recording";

// Violet marks the AI minutes only; red stays with BBB's recording.
export const ACCENT = "#A78BFA";
export const ACCENT_LIGHT = "#C4B5FD";
const ON_ACCENT = "#1B1530";

const SPARKLE = "M11 3.5l1.9 5.1 5.1 1.9-5.1 1.9L11 17.5l-1.9-5.1L4 10.5l5.1-1.9z";
const SPARKLE_SMALL = "M18.5 15l.8 2.2 2.2.8-2.2.8-.8 2.2-.8-2.2-2.2-.8 2.2-.8z";
const SPARKLE_FILLED = "M11 2.5l2.2 6 6 2.2-6 2.2-2.2 6-2.2-6-6-2.2 6-2.2z";

type Svg = SVGProps<SVGSVGElement>;

function size(px: number) {
  return { width: px, height: px, flexShrink: 0 };
}

// A ring that grows from radius r by 6 px and fades out in 1.8 s, like a box-shadow pulse. SMIL needs no stylesheet.
// Left out entirely under prefers-reduced-motion.
function pulse(cx: number, cy: number, r: number) {
  const timing = {
    dur: "1.8s",
    repeatCount: "indefinite",
    keyTimes: "0;0.7;1",
    calcMode: "spline",
    keySplines: "0 0 0.58 1;0 0 1 1",
  };
  return (
    <circle cx={cx} cy={cy} r={r} fill={ACCENT}>
      <animate attributeName="r" values={`${r};${r + 6};${r + 6}`} {...timing} />
      <animate attributeName="fill-opacity" values="0.55;0;0" {...timing} />
    </circle>
  );
}

// Off: outline sparkle.
export function sparkleOutlineIcon(px = 20) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke={ACCENT_LIGHT}
      strokeWidth="1.8"
      strokeLinejoin="round"
      aria-hidden="true"
      style={size(px)}
    >
      <path d={SPARKLE} />
      <path d={SPARKLE_SMALL} />
    </svg>
  );
}

// Running: filled violet circle with a sparkle and a pulsing ring.
function runningIcon(motion: boolean) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 22 22"
      aria-hidden="true"
      style={{ ...size(22), overflow: "visible" }}
    >
      {motion && pulse(11, 11, 11)}
      <circle cx="11" cy="11" r="11" fill={ACCENT} />
      <g transform="translate(4 4) scale(0.583333)">
        <path d={SPARKLE_FILLED} fill={ON_ACCENT} />
      </g>
    </svg>
  );
}

// Paused: outlined circle with a pause sign, no pulse.
function pausedIcon() {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 22 22" aria-hidden="true" style={size(22)}>
      <circle cx="11" cy="11" r="10.25" fill="none" stroke={ACCENT_LIGHT} strokeWidth="1.5" />
      <g transform="translate(6 6)">
        <rect x="1.5" y="1" width="2.4" height="8" rx="1" fill={ACCENT_LIGHT} />
        <rect x="6.1" y="1" width="2.4" height="8" rx="1" fill={ACCENT_LIGHT} />
      </g>
    </svg>
  );
}

// The chip's 8 px dot: filled and pulsing while running, hollow while paused.
function dot(state: "running" | "paused", motion: boolean) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 8 8"
      aria-hidden="true"
      style={{ ...size(8), overflow: "visible" }}
    >
      {state === "running" && motion && pulse(4, 4, 4)}
      {state === "running" ? (
        <circle cx="4" cy="4" r="4" fill={ACCENT} />
      ) : (
        <circle cx="4" cy="4" r="3.25" fill="none" stroke={ACCENT} strokeWidth="1.5" />
      )}
    </svg>
  );
}

// A 15 px info sign in the text colour, at the end of the chip's label.
export function infoIcon() {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      aria-hidden="true"
      style={size(15)}
    >
      <circle cx="12" cy="12" r="9" />
      <path d="M12 11v5M12 8h.01" />
    </svg>
  );
}

// The toast avatar: a violet circle with a sparkle that fills BBB's 2rem icon slot (common/toast/styles.js:86-122)
// and so covers its colour, which BBB takes from the toast type. The sparkle is centred and about as large as BBB's
// 1em icon glyph in that slot; its tips are thin, so it spans a little more than the glyph.
export function toastIcon(): Svg {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 32 32"
      aria-hidden="true"
      style={{ width: "2rem", height: "2rem" }}
    >
      <circle cx="16" cy="16" r="16" fill={ACCENT} />
      <g transform="translate(5 5.3)">
        <path d={SPARKLE_FILLED} fill={ON_ACCENT} />
      </g>
    </svg>
  ) as Svg;
}

export function chipDot(state: "running" | "paused", reducedMotion: boolean): Svg {
  return dot(state, !reducedMotion) as Svg;
}

// The SDK types an icon's svgContent as SVG props, but BBB renders it as a child (nav-bar/component.jsx:71-77), so it
// is an <svg> element.
export function pillIcon(state: MinutesState, reducedMotion: boolean): Svg {
  switch (state) {
    case "running":
      return runningIcon(!reducedMotion) as Svg;
    case "paused":
      return pausedIcon() as Svg;
    default:
      return sparkleOutlineIcon() as Svg;
  }
}

// Icons of the popovers, rendered by the plugin itself.
const rowIcon = {
  width: 18,
  height: 18,
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "#A7B0BC",
  strokeWidth: 2,
  strokeLinecap: "round",
  strokeLinejoin: "round",
  "aria-hidden": true,
  style: { flexShrink: 0, marginTop: 2 },
} as const;

export function micIcon() {
  return (
    <svg {...rowIcon}>
      <rect x="9" y="3" width="6" height="11" rx="3" />
      <path d="M5 11a7 7 0 0014 0M12 18v3" />
    </svg>
  );
}

export function chatIcon() {
  return (
    <svg {...rowIcon}>
      <path d="M4 5h16v11H9l-5 4z" />
    </svg>
  );
}

export function eyeIcon() {
  return (
    <svg {...rowIcon}>
      <path d="M3 12s3.5-6 9-6 9 6 9 6-3.5 6-9 6-9-6-9-6z" />
      <circle cx="12" cy="12" r="2.5" />
    </svg>
  );
}

export function sparkleFilledIcon(px: number, color: string) {
  return (
    <svg width={px} height={px} viewBox="0 0 24 24" fill={color} aria-hidden="true">
      <path d={SPARKLE_FILLED} />
    </svg>
  );
}

export function pauseFilledIcon(px: number, color: string) {
  return (
    <svg width={px} height={px} viewBox="0 0 24 24" fill={color} aria-hidden="true">
      <rect x="6" y="5" width="4" height="14" rx="1.5" />
      <rect x="14" y="5" width="4" height="14" rx="1.5" />
    </svg>
  );
}
