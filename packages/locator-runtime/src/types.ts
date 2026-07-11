export type TextMatcher = string | { text: string; exact?: boolean };

export interface Locator {
  by: "role" | "text" | "label" | "placeholder" | "testId" | "css";
  value?: string;
  role?: string;
  name?: TextMatcher;
  exact?: boolean;
  framePath?: Locator[];
  scope?: Locator;
  has?: Locator;
  hasText?: string;
  and?: Locator;
  or?: Locator;
  index?: number;
  nth?: number;
  first?: boolean;
  last?: boolean;
  shadow?: "auto" | "none";
}

export interface Rect {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface Actionability {
  attached: boolean;
  visible: boolean;
  enabled: boolean;
  stable: boolean;
  inViewport: boolean;
  receivesPointerEvents: boolean;
  reasons: string[];
}

export interface NodeDescription {
  nodeRef: string;
  elementIdentity: string;
  tag: string;
  role?: string;
  name?: string;
  text?: string;
  value?: string;
  sensitive?: boolean;
  attributes: Record<string, string>;
  rect: Rect;
  topRect?: Rect;
  canTranslateToTop: boolean;
  actionability: Actionability;
}

export interface SnapshotOptions {
  maxNodes?: number;
  includeHidden?: boolean;
  includeText?: boolean;
}

export interface RuntimeSnapshot {
  snapshotId: string;
  documentId: string;
  revision: number;
  url: string;
  title: string;
  aiDom: string;
  nodes: NodeDescription[];
  truncated: boolean;
  viewport: {
    width: number;
    height: number;
    devicePixelRatio: number;
    scrollX: number;
    scrollY: number;
    visualViewport?: {
      width: number;
      height: number;
      offsetLeft: number;
      offsetTop: number;
      scale: number;
    };
  };
}

export interface RuntimeViewport {
  width: number;
  height: number;
  devicePixelRatio: number;
  scrollX: number;
  scrollY: number;
  visualViewport?: {
    width: number;
    height: number;
    offsetLeft: number;
    offsetTop: number;
    scale: number;
  };
}

export interface PageAsset {
  kind: "image" | "media" | "font" | "stylesheet" | "script" | "svg" | "link";
  url?: string;
  mimeType?: string;
  text?: string;
  width?: number;
  height?: number;
}

export type RuntimeAction =
  | { type: "click" | "doubleClick" | "hover" | "focus" | "check" | "uncheck" }
  | { type: "fill" | "type" | "press"; value?: string }
  | { type: "select"; value?: string | string[] }
  | { type: "scroll"; value?: { x?: number; y?: number } };

export interface ActionResult {
  performed: boolean;
  revision: number;
  inputMode: "dom";
  target: NodeDescription;
}

export interface LocatorRuntimeApi {
  version: string;
  getRevision(): number;
  capture(options?: SnapshotOptions): RuntimeSnapshot;
  query(locator: Locator, snapshotId?: string): NodeDescription[];
  resolve(snapshotId: string, nodeRef: string): NodeDescription;
  prepare(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
  ): Promise<NodeDescription>;
  perform(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
    action: RuntimeAction,
    expectedElementIdentity?: string,
  ): Promise<ActionResult>;
  markSensitive(
    target: { locator: Locator } | { snapshotId: string; nodeRef: string },
    expectedElementIdentity?: string,
  ): NodeDescription;
  setSensitiveMask(enabled: boolean): void;
  viewport(): RuntimeViewport;
  exportContent(format: "html" | "text" | "markdown"): string;
  pageAssets(): PageAsset[];
  fullHtml(): string;
  dispose(): void;
}

declare global {
  interface Window {
    __browserControlLocatorRuntime?: LocatorRuntimeApi;
  }
}
