interface ClipboardMessage {
  target: "browser-control-offscreen";
  action: "read" | "write";
  text?: string;
  html?: string;
  items?: Array<{
    mimeType: string;
    data: string;
    encoding?: "base64" | "text";
  }>;
}

function fromBase64(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1)
    bytes[index] = binary.charCodeAt(index);
  return bytes;
}

function toBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

chrome.runtime.onMessage.addListener((raw: unknown, _sender, sendResponse) => {
  const message = raw as Partial<ClipboardMessage>;
  if (message.target !== "browser-control-offscreen") return false;
  void (async () => {
    if (message.action === "write") {
      const parts: Record<string, Blob> = {};
      if (message.text !== undefined)
        parts["text/plain"] = new Blob([message.text], { type: "text/plain" });
      if (message.html !== undefined)
        parts["text/html"] = new Blob([message.html], { type: "text/html" });
      for (const item of message.items ?? []) {
        const body =
          item.encoding === "base64" ? fromBase64(item.data) : item.data;
        parts[item.mimeType] = new Blob([body as BlobPart], {
          type: item.mimeType,
        });
      }
      await navigator.clipboard.write([new ClipboardItem(parts)]);
      return { ok: true, mimeTypes: Object.keys(parts) };
    }
    if (message.action === "read") {
      const items = await navigator.clipboard.read();
      const output: Array<{
        mimeType: string;
        data: string;
        encoding: "base64" | "text";
      }> = [];
      for (const item of items) {
        for (const type of item.types) {
          const blob = await item.getType(type);
          if (type.startsWith("text/"))
            output.push({
              mimeType: type,
              data: await blob.text(),
              encoding: "text",
            });
          else
            output.push({
              mimeType: type,
              data: toBase64(new Uint8Array(await blob.arrayBuffer())),
              encoding: "base64",
            });
        }
      }
      return { items: output };
    }
    throw new Error("unsupported offscreen action");
  })().then(sendResponse, (error: unknown) =>
    sendResponse({
      error: error instanceof Error ? error.message : String(error),
    }),
  );
  return true;
});
