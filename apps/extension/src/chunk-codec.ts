import type { JsonRpcMessage } from "./shared.js";
import { randomId } from "./shared.js";

const CHUNK_BYTES = 512 * 1024;
const MAX_DIRECT_BYTES = 1024 * 1024;
const MAX_ASSEMBLED_BYTES = 128 * 1024 * 1024;
const MAX_ASSEMBLIES = 32;
const ASSEMBLY_TTL_MS = 30_000;

export interface ChunkEnvelope {
  __bc_chunk: {
    version: 1;
    id: string;
    index: number;
    total: number;
    sha256: string;
    data: string;
  };
}

interface Assembly {
  createdAt: number;
  total: number;
  sha256: string;
  size: number;
  chunks: Array<Uint8Array | undefined>;
}

function bytesToBase64(bytes: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return btoa(binary);
}

function base64ToBytes(value: string): Uint8Array {
  const binary = atob(value);
  const output = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1)
    output[index] = binary.charCodeAt(index);
  return output;
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    bytes as Uint8Array<ArrayBuffer>,
  );
  return Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  if (left.byteLength !== right.byteLength) return false;
  for (let index = 0; index < left.byteLength; index += 1) {
    if (left[index] !== right[index]) return false;
  }
  return true;
}

export async function encodeMessage(
  message: JsonRpcMessage,
): Promise<Array<JsonRpcMessage | ChunkEnvelope>> {
  const bytes = new TextEncoder().encode(JSON.stringify(message));
  if (bytes.byteLength <= MAX_DIRECT_BYTES) return [message];
  if (bytes.byteLength > MAX_ASSEMBLED_BYTES)
    throw new Error(`native message exceeds ${MAX_ASSEMBLED_BYTES} bytes`);
  const id = randomId("chunk");
  const sha256 = await sha256Hex(bytes);
  const total = Math.ceil(bytes.byteLength / CHUNK_BYTES);
  const output: ChunkEnvelope[] = [];
  for (let index = 0; index < total; index += 1) {
    output.push({
      __bc_chunk: {
        version: 1,
        id,
        index,
        total,
        sha256,
        data: bytesToBase64(
          bytes.subarray(index * CHUNK_BYTES, (index + 1) * CHUNK_BYTES),
        ),
      },
    });
  }
  return output;
}

export function isChunkEnvelope(value: unknown): value is ChunkEnvelope {
  if (!value || typeof value !== "object") return false;
  const chunk = (value as Partial<ChunkEnvelope>).__bc_chunk;
  return (
    Boolean(chunk) &&
    chunk?.version === 1 &&
    typeof chunk.id === "string" &&
    Number.isSafeInteger(chunk.index) &&
    Number.isSafeInteger(chunk.total) &&
    /^[a-f0-9]{64}$/.test(chunk.sha256) &&
    typeof chunk.data === "string"
  );
}

export class ChunkAssembler {
  readonly #assemblies = new Map<string, Assembly>();

  async accept(value: unknown): Promise<JsonRpcMessage | undefined> {
    this.#prune();
    if (!isChunkEnvelope(value)) return value as JsonRpcMessage;
    const chunk = value.__bc_chunk;
    if (
      chunk.total < 1 ||
      chunk.total > 4_096 ||
      chunk.index < 0 ||
      chunk.index >= chunk.total
    ) {
      throw new Error("invalid chunk envelope");
    }
    let assembly = this.#assemblies.get(chunk.id);
    if (!assembly) {
      if (this.#assemblies.size >= MAX_ASSEMBLIES)
        throw new Error("too many incomplete chunk streams");
      assembly = {
        createdAt: Date.now(),
        total: chunk.total,
        sha256: chunk.sha256,
        size: 0,
        chunks: new Array(chunk.total),
      };
      this.#assemblies.set(chunk.id, assembly);
    }
    if (assembly.total !== chunk.total || assembly.sha256 !== chunk.sha256) {
      this.#assemblies.delete(chunk.id);
      throw new Error("chunk metadata changed within stream");
    }
    const decoded = base64ToBytes(chunk.data);
    const previous = assembly.chunks[chunk.index];
    if (previous && !equalBytes(previous, decoded)) {
      this.#assemblies.delete(chunk.id);
      throw new Error("duplicate chunk data changed within stream");
    }
    if (!previous) {
      assembly.chunks[chunk.index] = decoded;
      assembly.size += decoded.byteLength;
    }
    if (assembly.size > MAX_ASSEMBLED_BYTES) {
      this.#assemblies.delete(chunk.id);
      throw new Error(
        `reassembled native message exceeds ${MAX_ASSEMBLED_BYTES} bytes`,
      );
    }
    for (let index = 0; index < assembly.total; index += 1) {
      if (assembly.chunks[index] === undefined) return undefined;
    }

    const joined = new Uint8Array(assembly.size);
    let offset = 0;
    for (const part of assembly.chunks) {
      joined.set(part!, offset);
      offset += part!.byteLength;
    }
    this.#assemblies.delete(chunk.id);
    if ((await sha256Hex(joined)) !== assembly.sha256)
      throw new Error("native message chunk checksum mismatch");
    return JSON.parse(new TextDecoder().decode(joined)) as JsonRpcMessage;
  }

  clear(): void {
    this.#assemblies.clear();
  }

  #prune(): void {
    const cutoff = Date.now() - ASSEMBLY_TTL_MS;
    for (const [streamId, assembly] of this.#assemblies) {
      if (assembly.createdAt < cutoff) this.#assemblies.delete(streamId);
    }
  }
}
