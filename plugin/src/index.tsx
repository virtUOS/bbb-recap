// Entry point. BBB adds this script with the attributes uuid and pluginName and an empty <div id={uuid}> for the
// plugin (bigbluebutton-html5/imports/ui/components/plugins-engine/loader/manager.tsx:30-52 @ 7d27e7e).
import { pluginLogger } from "bigbluebutton-html-plugin-sdk";
import { createRoot } from "react-dom/client";

import RecapPlugin from "./recap-plugin/component";
import { isRecapEnabled } from "./recap-plugin/recording";

const script = document.currentScript;
const uuid = script?.getAttribute("uuid") || "root";
const container = document.getElementById(uuid);

if (!isRecapEnabled(script?.getAttribute("src"))) {
  pluginLogger.info("recap plugin idle: meta_recap-enabled is not true in this meeting");
} else if (container) {
  createRoot(container).render(<RecapPlugin pluginUuid={uuid} />);
}
