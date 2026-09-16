// The only two jobs are the install criterion and caching static assets.
const CACHE = "dl-tool-assets-v1";
// registration.scope is an absolute URL ending in "/", so this resolves to
// the Vite output directory under the installed base path.
const ASSETS_PATH = new URL("assets/", self.registration.scope).pathname;

self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((k) =>
        Promise.all(k.filter((n) => n !== CACHE).map(caches.delete, caches)),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (e) => {
  const url = new URL(e.request.url);
  const isAsset =
    e.request.method === "GET" &&
    url.origin === self.location.origin &&
    url.pathname.startsWith(ASSETS_PATH);
  // API, SSE and index.html always go to the network.
  if (!isAsset) return;
  e.respondWith(
    caches.open(CACHE).then(async (c) => {
      const hit = await c.match(e.request);
      if (hit) return hit;
      const res = await fetch(e.request);
      if (res.ok) await c.put(e.request, res.clone());
      return res;
    }),
  );
});
