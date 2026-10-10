const port = Number(Bun.argv[2]);

// index.html is read on every request, so an edit from the harness shows on the next reload
// without a restart.
Bun.serve({
  hostname: "127.0.0.1",
  port,
  routes: {
    "/": () => new Response(Bun.file("index.html"), { headers: { "Content-Type": "text/html; charset=utf-8" } }),
    "/up": new Response("ok"),
  },
  fetch: () => new Response("Not found", { status: 404 }),
});

console.log(`vibe demo listening on ${port}`);
