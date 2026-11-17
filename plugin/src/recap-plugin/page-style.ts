// The one stylesheet the plugin adds to the page, while it is active (only in recap meetings, index.tsx). Each rule
// targets as little as possible; see docs/operations/install-plugin.md.
import { POPOVER_ATTRIBUTE } from "./popover";

export const PAGE_CSS = [
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
