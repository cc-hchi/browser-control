#!/usr/bin/env node

import { createServer } from "node:net";

function listen(port) {
  return new Promise((resolve, reject) => {
    const server = createServer();
    server.once("error", reject);
    server.listen(port, "127.0.0.1", () => resolve(server));
  });
}

function close(server) {
  return new Promise((resolve, reject) => {
    server.close(error => (error ? reject(error) : resolve()));
  });
}

for (let attempt = 0; attempt < 100; attempt += 1) {
  const base = 20_000 + Math.floor(Math.random() * 20_000);
  let first;
  let second;
  try {
    first = await listen(base);
    second = await listen(base + 1);
    await close(second);
    await close(first);
    process.stdout.write(`${base}\n`);
    process.exit(0);
  } catch {
    if (second) await close(second).catch(() => {});
    if (first) await close(first).catch(() => {});
  }
}

throw new Error("could not reserve an adjacent localhost port pair");
