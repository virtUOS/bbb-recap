// A popover under a nav bar item, in the style of BBB's dark popovers. A native modal <dialog> on document.body: the
// plugin's own container sits behind the client (plugins-engine/styles.ts:10 @ 7d27e7e), and showModal() puts the
// dialog on top, keeps the focus inside, closes on Escape and gives the focus back to the nav bar item on close
// (https://html.spec.whatwg.org/multipage/interactive-elements.html#the-dialog-element). The SDK has no anchored
// popover of its own.
import { type CSSProperties, type ReactNode, useId, useLayoutEffect, useRef } from "react";
import { createPortal } from "react-dom";

// Marks the plugin's dialogs for the page style (page-style.ts), which makes their backdrop transparent.
export const POPOVER_ATTRIBUTE = "data-recap-popover";

const MARGIN = 8;

// Where the popover goes: below the anchor, right edges aligned, inside the viewport. Without an anchor, top right.
export function popoverPosition(
  anchor: { bottom: number; right: number } | null,
  width: number,
  viewportWidth: number,
): { top: number; left: number; width: number } {
  const w = Math.min(width, viewportWidth - 2 * MARGIN);
  if (!anchor) {
    return { top: 64, left: Math.max(MARGIN, viewportWidth - w - 16), width: w };
  }
  const left = Math.min(Math.max(MARGIN, anchor.right - w), viewportWidth - w - MARGIN);
  return { top: anchor.bottom + MARGIN, left, width: w };
}

export const colors = {
  text: "#E8EAED",
  body: "#C5CBD3",
  muted: "#A7B0BC",
  primary: "var(--color-primary, #0F70D7)",
};

const dialogStyle: CSSProperties = {
  position: "fixed",
  margin: 0,
  right: "auto",
  bottom: "auto",
  padding: 0,
  border: "none",
  borderRadius: 12,
  boxShadow: "0 12px 32px rgba(0, 0, 0, 0.5)",
  background: "#23282F",
  color: colors.text,
  fontFamily: "inherit",
  textAlign: "start",
};

const buttonBase: CSSProperties = {
  display: "inline-flex",
  alignItems: "center",
  gap: 8,
  height: 44,
  padding: "0 18px",
  borderRadius: 22,
  border: 0,
  font: "inherit",
  fontSize: 15,
  fontWeight: 600,
  cursor: "pointer",
};

export const buttonStyles = {
  text: { ...buttonBase, background: "transparent", color: "#DCE0E5" },
  primary: { ...buttonBase, padding: "0 20px 0 16px", background: colors.primary, color: "#fff" },
  secondary: { ...buttonBase, height: 40, borderRadius: 20, fontSize: 14, background: "#2A3038", color: colors.text },
} satisfies Record<string, CSSProperties>;

interface PopoverProps {
  // data-test of the nav bar item it belongs to.
  anchor: string;
  width: number;
  padding: string;
  title: ReactNode;
  titleStyle: CSSProperties;
  titleIcon?: ReactNode;
  children: ReactNode;
  actions: ReactNode;
  onClose: () => void;
}

export default function Popover({
  anchor,
  width,
  padding,
  title,
  titleStyle,
  titleIcon,
  children,
  actions,
  onClose,
}: PopoverProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  // A layout effect, so close() runs before React removes the dialog and the focus goes back to the nav bar item.
  useLayoutEffect(() => {
    const dialog = ref.current;
    if (!dialog) {
      return;
    }
    const place = () => {
      const rect = document.querySelector(`[data-test="${anchor}"]`)?.getBoundingClientRect() ?? null;
      const { top, left, width: w } = popoverPosition(rect, width, window.innerWidth);
      dialog.style.top = `${top}px`;
      dialog.style.left = `${left}px`;
      dialog.style.width = `${w}px`;
    };
    place();
    dialog.showModal();
    window.addEventListener("resize", place);
    return () => {
      window.removeEventListener("resize", place);
      dialog.close();
      hideNavItemTooltip(anchor);
    };
  }, [anchor, width]);

  return createPortal(
    <dialog
      ref={ref}
      {...{ [POPOVER_ATTRIBUTE]: "" }}
      aria-labelledby={titleId}
      style={dialogStyle}
      onClose={() => {
        hideNavItemTooltip(anchor);
        onClose();
      }}
      // A click on the dialog itself, not on its content, is a click on the backdrop: close, like a popover.
      onClick={(event) => event.target === event.currentTarget && ref.current?.close()}
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 14, padding, boxSizing: "border-box" }}>
        <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
          {titleIcon}
          <h2 id={titleId} style={{ margin: 0, lineHeight: 1.25, ...titleStyle }}>
            {title}
          </h2>
        </div>
        {children}
        <div style={{ display: "flex", justifyContent: "flex-end", gap: 8, marginTop: 4 }}>{actions}</div>
      </div>
    </dialog>,
    document.body,
  );
}

// BBB's tooltip on a nav bar button is a tippy.js 6.3.7 instance with the default trigger "mouseenter focus"
// (common/tooltip/component.jsx:79-109 @ 7d27e7e, same in v4.0.2; tippy.js dist/tippy.cjs.js:378). Closing a modal dialog
// returns the focus to the pill or chip, as it should, and that focus event schedules the tooltip, which then stays
// until the focus leaves the button: under the nav bar, over the toasts. So after a popover closes, the pending or
// shown tooltip is cancelled; the focus stays on the button. Screen readers are not affected: BBB gives the tooltip
// no ARIA role (aria: null) and sets the same text as the button's aria-label (nav-bar/component.jsx:85). tippy.js
// documents the instance on the reference element as _tippy (https://atomiks.github.io/tippyjs/v6/tippy-instance/),
// and BBB reads it the same way (common/tooltip/component.jsx:117-118). Without an instance, nothing happens.
export function hideTooltipOf(element: Element | null) {
  const tippy = (element as { _tippy?: { clearDelayTimeouts?: () => void; hide?: () => void } } | null)?._tippy;
  tippy?.clearDelayTimeouts?.();
  tippy?.hide?.();
}

// The same for a nav bar item, found by its data-test. Called after every click on the pill or chip, since a mouse
// click focuses the button and the focus trigger would keep the tooltip open after the pointer has left; and when a
// toast is sent, so a tooltip never covers it. Focus by Tab still shows the tooltip until the next of these.
export function hideNavItemTooltip(dataTest: string) {
  hideTooltipOf(globalThis.document?.querySelector(`[data-test="${dataTest}"]`) ?? null);
}

// Closes the dialog the button is in; the dialog's close event then tells the owner.
export function closeDialogOf(button: HTMLElement) {
  button.closest("dialog")?.close();
}
