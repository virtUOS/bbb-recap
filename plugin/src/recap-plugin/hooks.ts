import { useEffect, useState, useSyncExternalStore } from "react";

import { type CurrentTimeData, serverOffset } from "./recording";

const REDUCED_MOTION = "(prefers-reduced-motion: reduce)";

function subscribeReducedMotion(onChange: () => void): () => void {
  const query = window.matchMedia?.(REDUCED_MOTION);
  query?.addEventListener("change", onChange);
  return () => query?.removeEventListener("change", onChange);
}

function reducedMotionNow(): boolean {
  return window.matchMedia?.(REDUCED_MOTION).matches === true;
}

// True while the user's system asks for reduced motion; follows changes live. BBB's own "animations" setting is not
// available to plugins.
export function useReducedMotion(): boolean {
  return useSyncExternalStore(subscribeReducedMotion, reducedMotionNow, () => false);
}

// The client clock, updated every second. The plugin re-renders with it; the nav bar is updated only when the shown
// time changes.
export function useClock(): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}

// Server time minus client time in milliseconds, from the answer to a current_time query that was sent when the
// plugin mounted. 0 until the answer is there.
export function useServerOffset(data: CurrentTimeData | null | undefined): number {
  const [sentAt] = useState(() => Date.now());
  const [offset, setOffset] = useState(0);
  useEffect(() => {
    if (data) {
      // The arrival time of the answer is known only now, when it has arrived.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setOffset(serverOffset(data, sentAt, Date.now()));
    }
  }, [data, sentAt]);
  return offset;
}
