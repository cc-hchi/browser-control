import { createLocatorRuntime } from "../../../packages/locator-runtime/src/index.js";

const existing = window.__browserControlLocatorRuntime;
if (!existing || existing.version !== "1.0.0") {
  existing?.dispose();
  window.__browserControlLocatorRuntime = createLocatorRuntime();
}

if (
  window === window.top &&
  !document.getElementById("__browser_control_indicator")
) {
  const host = document.createElement("div");
  host.id = "__browser_control_indicator";
  Object.assign(host.style, {
    all: "initial",
    position: "fixed",
    top: "8px",
    right: "8px",
    zIndex: "2147483647",
    pointerEvents: "none",
  });
  const shadow = host.attachShadow({ mode: "closed" });
  const badge = document.createElement("div");
  badge.textContent = "AI control active";
  Object.assign(badge.style, {
    font: "600 11px/1 system-ui, sans-serif",
    color: "white",
    background: "rgba(37, 99, 235, .9)",
    borderRadius: "999px",
    padding: "6px 9px",
    boxShadow: "0 2px 8px rgba(0,0,0,.22)",
  });
  shadow.append(badge);
  document.documentElement.append(host);
}
