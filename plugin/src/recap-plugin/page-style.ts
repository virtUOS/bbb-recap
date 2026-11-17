// The one stylesheet the plugin adds to the page, while it is active (only in recap meetings, index.tsx). Each rule
// targets as little as possible; see docs/operations/install-plugin.md.
import { POPOVER_ATTRIBUTE } from "./popover";

// BBB's toasts for a started and a paused recording. In recap meetings the recording is the AI minutes, and the
// plugin shows its own toasts for both changes. The server sends them as notifications with type, icon and message
// id (akka-bbb-apps/.../core/apps/users/SetRecordingStatusCmdMsgHdlr.scala:51-58,79-85 @ 7d27e7e); the client turns
// these into the toast id "<type>-<icon>-<message id>" (bigbluebutton-html5/imports/ui/services/notification/
// index.js:11-38, notifications/component.tsx:103-112), and react-toastify 11.0.2 puts that id on the toast's element
// (src/components/Toast.tsx, <div id={toastId}>). Each selector matches exactly one toast. If BBB changes the ids, the
// rules match nothing and BBB's toasts simply show again. Same in BBB v4.0.2.
// Not display:none: react-toastify closes a toast when its progress bar animation ends (src/components/
// ProgressBar.tsx, onAnimationEnd) and removes it after its exit animation, and no animation runs under display:none,
// so the toasts would stay open, unseen. Invisible and out of the flow, they still close and go away as usual, and
// screen readers do not announce them.
export const HIDDEN_BBB_TOASTS = [
  "success-record-app.notification.recordingStart",
  "error-record-app.notification.recordingPaused",
];

export const PAGE_CSS = [
  `${HIDDEN_BBB_TOASTS.map((id) => `[id="${id}"]`).join(", ")} ` +
    "{ position: absolute !important; visibility: hidden !important; pointer-events: none !important; }",
  // The plugin's own popovers are modal dialogs; like a popover, they do not dim the page.
  `dialog[${POPOVER_ATTRIBUTE}]::backdrop { background: transparent; }`,
].join("\n");

// Adds the stylesheet to <head>; the returned function removes it.
export function installPageStyle(): () => void {
  const style = document.createElement("style");
  style.setAttribute("data-recap-plugin", "");
  style.textContent = PAGE_CSS;
  document.head.appendChild(style);
  return () => style.remove();
}
