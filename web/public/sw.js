// The only two jobs are the install criterion and caching static assets.
// CacheStorage is shared by the whole origin, so the cache name carries the
// deployment scope: activation must only ever delete caches this deployment
// itself created, never a neighbour application's. The ":" terminator keeps
// "/dl-tool/" and "/dl-tool-extras/" in distinct namespaces.
const CACHE_PREFIX = `dl-tool@${new URL(self.registration.scope).pathname}:`;
const CACHE = `${CACHE_PREFIX}assets-v1`;
// Deployments from before the scoped name existed used this one; only
// dl-tool ever wrote to it, so activation may reclaim it.
const LEGACY_CACHE = "dl-tool-assets-v1";
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
        Promise.all(
          k
            .filter(
              (n) =>
                n === LEGACY_CACHE ||
                (n.startsWith(CACHE_PREFIX) && n !== CACHE),
            )
            .map(caches.delete, caches),
        ),
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
