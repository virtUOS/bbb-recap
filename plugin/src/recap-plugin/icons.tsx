// SVG icons. The nav bar and toast icons are rendered by BBB's React, so they are plain SVG elements only, no
// components or hooks (the plugin bundles its own React copy). BBB forces nav bar icons to 1em
// (plugins/plugin-icon/styles.tsx:10-13 @ 7d27e7e); an inline width and height override that.
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
export function sparkleOutlineIcon(px = 20): Svg {
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
  ) as Svg;
}

// Running: filled violet circle with a sparkle and a pulsing ring.
function runningIcon(motion: boolean): Svg {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 22 22"
      aria-hidden="true"
      style={{ ...size(22), overflow: "visible" }}
    >
      {motion && pulse(11, 11, 11)}
      <circle cx="11" cy="11" r="11" fill={ACCENT} />
      <svg x="4" y="4" width="14" height="14" viewBox="0 0 24 24">
        <path d={SPARKLE_FILLED} fill={ON_ACCENT} />
      </svg>
    </svg>
  ) as Svg;
}

// Paused: outlined circle with a pause sign, no pulse.
function pausedIcon(): Svg {
  return (
    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 22 22" aria-hidden="true" style={size(22)}>
      <circle cx="11" cy="11" r="10.25" fill="none" stroke={ACCENT_LIGHT} strokeWidth="1.5" />
      <svg x="6" y="6" width="10" height="10" viewBox="0 0 10 10">
        <rect x="1.5" y="1" width="2.4" height="8" rx="1" fill={ACCENT_LIGHT} />
        <rect x="6.1" y="1" width="2.4" height="8" rx="1" fill={ACCENT_LIGHT} />
      </svg>
    </svg>
  ) as Svg;
}

export function pillIcon(state: MinutesState, reducedMotion: boolean): Svg {
  switch (state) {
    case "running":
      return runningIcon(!reducedMotion);
    case "paused":
      return pausedIcon();
    default:
      return sparkleOutlineIcon();
  }
}
