import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(fileURLToPath(new URL(".", import.meta.url)), "site");
const basePort = Number.parseInt(process.env.FIXTURE_PORT ?? "4173", 10);

const types = new Map([
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".css", "text/css; charset=utf-8"],
  [".txt", "text/plain; charset=utf-8"],
]);

function handler(request, response) {
  const url = new URL(request.url ?? "/", "http://fixture.local");
  response.setHeader("Access-Control-Allow-Origin", "*");

  if (url.pathname === "/download/report.txt") {
    response.writeHead(200, {
      "Content-Disposition": 'attachment; filename="report.txt"',
      "Content-Type": "text/plain; charset=utf-8",
    });
    response.end("browser-control fixture report\n");
    return;
  }

  if (url.pathname === "/redirect") {
    response.writeHead(302, { Location: "/#redirected" });
    response.end();
    return;
  }

  if (url.pathname === "/api/echo" && request.method === "POST") {
    const chunks = [];
    request.on("data", chunk => chunks.push(chunk));
    request.on("end", () => {
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify({ size: Buffer.concat(chunks).length }));
    });
    return;
  }

  let relative;
  if (url.pathname === "/") relative = "index.html";
  else if (url.pathname === "/popup") relative = "popup.html";
  else if (url.pathname.startsWith("/frame/")) relative = "frame.html";
  else relative = url.pathname.slice(1);

  const safe = normalize(relative).replace(/^(\.\.(\/|\\|$))+/, "");
  const path = join(root, safe);
  if (!path.startsWith(root) || !existsSync(path) || !statSync(path).isFile()) {
    response.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" });
    response.end("not found\n");
    return;
  }

  response.writeHead(200, {
    "Cache-Control": "no-store",
    "Content-Type": types.get(extname(path)) ?? "application/octet-stream",
  });
  createReadStream(path).pipe(response);
}

const servers = [basePort, basePort + 1].map(port => {
  const server = createServer(handler);
  server.listen(port, "127.0.0.1", () => {
    process.stderr.write(`fixture server listening on http://127.0.0.1:${port}\n`);
  });
  return server;
});

function close() {
  for (const server of servers) server.close();
}

process.on("SIGINT", close);
process.on("SIGTERM", close);
