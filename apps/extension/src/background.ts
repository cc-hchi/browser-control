import { NativeBridge } from "./native-bridge.js";
import { ExtensionService } from "./service.js";

const service = new ExtensionService();
const bridge = new NativeBridge(
  service.handleRequest,
  service.handleNotification,
  (state) => service.updateBridgeState(state),
  () => service.failClosed(),
);

service.attachBridge(bridge);

void service.initialize().then(
  () => bridge.start(),
  (error: unknown) => {
    const message = error instanceof Error ? error.message : String(error);
    void chrome.action.setTitle({
      title: `Local Chrome Control — failed to start: ${message}`,
    });
  },
);

chrome.runtime.onSuspend.addListener(() => {
  bridge.stop();
});
