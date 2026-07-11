import { randomUUID } from "node:crypto";
import { spawnSync } from "node:child_process";

const browserctl = process.env.BROWSERCTL_BIN;
if (!browserctl) throw new Error("BROWSERCTL_BIN is required");
const fixturePort = Number.parseInt(process.env.FIXTURE_PORT ?? "4173", 10);
const fixtureOrigin = `http://127.0.0.1:${fixturePort}`;

function invoke(args, expectedErrorKind) {
  const child = spawnSync(browserctl, ["--json", "--timeout", "90s", ...args], { encoding: "utf8" });
  let response;
  try {
    response = JSON.parse(child.stdout);
  } catch {
    throw new Error(`browserctl returned invalid JSON (${child.status}): ${child.stdout}\n${child.stderr}`);
  }
  if (expectedErrorKind) {
    const kind = response?.error?.data?.kind;
    if (child.status === 0 || kind !== expectedErrorKind) {
      throw new Error(`expected ${expectedErrorKind}, got status ${child.status}: ${child.stdout}`);
    }
    return response.error;
  }
  if (child.status !== 0 || response.error) {
    throw new Error(`browserctl failed (${child.status}): ${child.stdout}\n${child.stderr}`);
  }
  return response.result ?? response;
}

function rpc(method, params = {}, expectedErrorKind) {
  return invoke(["rpc", method, "--params", JSON.stringify(params)], expectedErrorKind);
}

function operationId() {
  return `op_${randomUUID().replaceAll("-", "")}`;
}

const doctor = invoke(["doctor"]);
if (doctor.ok !== true) throw new Error("doctor did not report healthy");

const session = rpc("session.open", { name: "chrome-e2e", clientId: "acceptance" });
let stopped = false;
let browserInstanceId;
try {
  let fixtureTab;
  for (let attempt = 0; attempt < 120; attempt += 1) {
    const diagnostics = rpc("daemon.diagnostics", {});
    const matches = [];
    for (const browser of diagnostics.browsers ?? []) {
      const candidateId = browser.browserInstanceId;
      if (typeof candidateId !== "string") continue;
      const listed = rpc("tab.list", { browserInstanceId: candidateId });
      const tab = listed.tabs?.find((item) => String(item.url).startsWith(`${fixtureOrigin}/`));
      if (tab) matches.push({ browserInstanceId: candidateId, tab });
    }
    if (matches.length > 1) throw new Error("fixture tab appeared in more than one Chrome instance");
    if (matches.length === 1) {
      browserInstanceId = matches[0].browserInstanceId;
      fixtureTab = matches[0].tab;
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  if (!fixtureTab) throw new Error("fixture tab did not appear");

  const claim = rpc("tab.claim", {
    sessionId: session.sessionId,
    browserInstanceId,
    tabId: fixtureTab.tabId,
  });
  const base = {
    sessionId: session.sessionId,
    browserInstanceId,
    tabId: fixtureTab.tabId,
    leaseId: claim.leaseId,
  };
  const claimedMetadata = rpc("tab.list", { browserInstanceId }).tabs?.find(
    (tab) => tab.tabId === fixtureTab.tabId,
  );
  if (
    claimedMetadata?.claimed !== true ||
    "claim" in claimedMetadata ||
    "leaseId" in claimedMetadata ||
    "sessionId" in claimedMetadata
  ) {
    throw new Error(`public tab metadata leaked claim credentials: ${JSON.stringify(claimedMetadata)}`);
  }
  let observation = rpc("observation.capture", { ...base, options: { screenshot: false } });
  let epoch = observation.documentEpoch;
  if (!observation.snapshotId || !Number.isInteger(epoch)) throw new Error("observation did not return snapshot identity");

  rpc("action.perform", {
    ...base,
    operationId: operationId(),
    expectedDocumentEpoch: epoch,
    action: { type: "fill", target: { locator: { by: "label", value: "Email", exact: true } }, value: "e2e@example.test" },
  });
  const structured = rpc("observation.capture", {
    ...base,
    options: { screenshot: false, fullDom: true, accessibility: true },
  });
  if (JSON.stringify(structured.fullDom).includes("e2e@example.test"))
    throw new Error("full DOM observation exposed a live form value");
  const accessibilityJSON = JSON.stringify(structured.accessibility);
  if (accessibilityJSON.includes("e2e@example.test")) {
    const index = accessibilityJSON.indexOf("e2e@example.test");
    throw new Error(
      `accessibility observation exposed a live form value near ${accessibilityJSON.slice(Math.max(0, index - 240), index + 240)}`,
    );
  }
  const exported = rpc("content.export", {
    ...base,
    operationId: operationId(),
    format: "dom",
  });
  const artifactId = exported.result?.artifactId ?? exported.artifactId;
  if (typeof artifactId !== "string")
    throw new Error(`DOM export omitted artifactId: ${JSON.stringify(exported)}`);
  const chunk = rpc("artifact.readChunk", {
    sessionId: session.sessionId,
    artifactId,
    offset: 0,
    length: 512 * 1024,
  });
  const exportedDom = Buffer.from(chunk.dataBase64, "base64").toString("utf8");
  if (exportedDom.includes("e2e@example.test"))
    throw new Error("DOM artifact exposed a live form value");
  rpc("artifact.delete", { sessionId: session.sessionId, artifactId });
  const visual = rpc("observation.capture", {
    ...base,
    options: { screenshot: { format: "png" } },
  });
  if (
    typeof visual.screenshot?.data !== "string" ||
    visual.screenshot.data.length < 100 ||
    !Number.isFinite(visual.screenshot.pixelWidth) ||
    !Number.isFinite(visual.screenshot.pixelHeight)
  ) {
    throw new Error("viewport screenshot omitted PNG data or pixel dimensions");
  }
  rpc("action.perform", {
    ...base,
    operationId: operationId(),
    expectedDocumentEpoch: epoch,
    action: { type: "click", target: { locator: { by: "role", role: "button", name: { text: "Shadow action", exact: true } } } },
  });
  const shadowResult = rpc("locator.query", {
    ...base,
    locator: { by: "text", value: "shadow clicked", exact: true },
  });
  if (shadowResult.count !== 1) throw new Error(`shadow action result count = ${shadowResult.count}`);

  const nestedFrame = rpc("locator.query", {
    ...base,
    locator: {
      by: "label",
      value: "Frame input",
      exact: true,
      framePath: [
        { by: "css", value: "#cross-origin-frame" },
        { by: "css", value: "#nested" },
      ],
    },
  });
  if (nestedFrame.count !== 1) throw new Error(`three-level frame query count = ${nestedFrame.count}`);

  rpc("action.perform", {
    ...base,
    operationId: operationId(),
    expectedDocumentEpoch: epoch,
    action: { type: "click", target: { locator: { by: "role", role: "button", name: { text: "Same-document navigation", exact: true } } } },
    expect: [{ type: "navigation", timeoutMs: 10_000 }],
  });
  observation = rpc("observation.capture", { ...base, options: { screenshot: false } });
  epoch = observation.documentEpoch;

  const confirmationOperation = operationId();
  const confirmationError = rpc("action.perform", {
    ...base,
    operationId: confirmationOperation,
    expectedDocumentEpoch: epoch,
    action: { type: "click", target: { locator: { by: "testId", value: "submit-profile" } } },
    confirmation: { required: true, reason: "submit fixture profile" },
  }, "CONFIRMATION_REQUIRED");
  const confirmationId = confirmationError.data?.confirmationId;
  if (!confirmationId) throw new Error("confirmation error omitted confirmationId");
  const confirmation = rpc("confirmation.get", {
    sessionId: session.sessionId,
    confirmationId,
  });
  if (confirmation.status !== "pending" || confirmation.requestHash?.length !== 64) {
    throw new Error(`invalid pending confirmation: ${JSON.stringify(confirmation)}`);
  }

  rpc("browser.stop", { sessionId: session.sessionId, browserInstanceId, reason: "chrome e2e complete" });
  stopped = true;
  process.stdout.write(`${JSON.stringify({ ok: true, browserInstanceId, tabId: fixtureTab.tabId, confirmationId })}\n`);
} finally {
  if (!stopped) {
    try {
      rpc("browser.stop", { sessionId: session.sessionId, browserInstanceId, reason: "chrome e2e cleanup" });
    } catch {
      // Preserve the original E2E failure.
    }
  }
}
