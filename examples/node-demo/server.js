const http = require("node:http");

function parseItems(raw) {
  if (typeof raw !== "string" || raw.trim() === "") {
    return [];
  }
  // Intentional bug: fails to filter empty segments after trailing commas.
  return raw.split(",").map((s) => s.trim());
}

function createServer() {
  return http.createServer((req, res) => {
    const u = new URL(req.url, "http://127.0.0.1:3000");
    if (u.pathname === "/health") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ ok: true, env: process.env.NODE_ENV || "dev" }));
      return;
    }
    if (u.pathname === "/items") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ items: parseItems(u.searchParams.get("q") || "") }));
      return;
    }
    res.writeHead(404);
    res.end("not found\n");
  });
}

if (require.main === module) {
  const port = Number(process.env.PORT || 3000);
  createServer().listen(port, "127.0.0.1", () => {
    console.log(`node-demo listening on 127.0.0.1:${port}`);
  });
}

module.exports = { parseItems, createServer };
