import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import vm from "node:vm";
import { expect, test, vi } from "vitest";

// public/sw.js is a plain worker script, not a module: run it in a vm
// context against fakes for the globals it consumes — self, caches, fetch —
// and capture the handlers it registers.
const source = readFileSync(resolve(process.cwd(), "public/sw.js"), "utf8");

interface FakeWorker {
  handlers: Map<string, (event: unknown) => unknown>;
  store: Map<string, Map<string, Response>>;
  fetch: ReturnType<typeof vi.fn>;
}

function loadWorker(scope: string): FakeWorker {
  const handlers = new Map<string, (event: unknown) => unknown>();
  const store = new Map<string, Map<string, Response>>();
  const caches = {
    keys: () => Promise.resolve([...store.keys()]),
    delete: (name: string) => Promise.resolve(store.delete(name)),
    open: (name: string) => {
      if (!store.has(name)) store.set(name, new Map());
      const box = store.get(name)!;
      return Promise.resolve({
        match: (request: Request) => Promise.resolve(box.get(request.url)),
        put: (request: Request, response: Response) => {
          box.set(request.url, response);
          return Promise.resolve();
        },
      });
    },
  };
  const self = {
    registration: { scope },
    location: new URL(scope),
    addEventListener: (type: string, handler: (event: unknown) => unknown) =>
      handlers.set(type, handler),
    skipWaiting: vi.fn(),
    clients: { claim: vi.fn(() => Promise.resolve()) },
  };
  const fetch = vi.fn();
  vm.runInNewContext(source, { self, caches, fetch, URL, Promise });
  return { handlers, store, fetch };
}

test("TestActivationDeletesOnlyOwnScopedCaches", async () => {
  const worker = loadWorker("https://nas.example/dl-tool/");
  worker.store.set("other-app-offline-v4", new Map());
  worker.store.set("dl-tool@/dl-tool/:assets-v1", new Map());
  worker.store.set("dl-tool@/dl-tool/:assets-v0", new Map());
  worker.store.set("dl-tool@/other/:assets-v9", new Map());
  const activate = worker.handlers.get("activate")!;
  // waitUntil's promise is the deletions plus clients.claim().
  await activate({ waitUntil: (pending: Promise<unknown>) => pending });
  expect([...worker.store.keys()].sort()).toEqual([
    "dl-tool@/dl-tool/:assets-v1",
    "dl-tool@/other/:assets-v9",
    "other-app-offline-v4",
  ]);
});

test("TestFetchCachesScopedAssetsOnly", async () => {
  const worker = loadWorker("https://nas.example/dl-tool/");
  worker.fetch.mockResolvedValue(new Response("body", { status: 200 }));
  const respondWith = vi.fn((pending: Promise<Response>) => pending);
  const request = new Request("https://nas.example/dl-tool/assets/app.js");
  const onFetch = worker.handlers.get("fetch")!;
  onFetch({ request, respondWith });
  const first = (await respondWith.mock.results[0].value) as Response;
  expect(first.status).toBe(200);
  expect(
    worker.store.get("dl-tool@/dl-tool/:assets-v1")?.has(request.url),
  ).toBe(true);
  onFetch({ request, respondWith });
  await respondWith.mock.results[1].value;
  // The repeat request is a cache hit: no second network call.
  expect(worker.fetch).toHaveBeenCalledTimes(1);
  // API and cross-origin requests are never intercepted.
  for (const url of [
    "https://nas.example/dl-tool/api/v1/tasks",
    "https://other.example/dl-tool/assets/app.js",
  ]) {
    const passthrough = vi.fn();
    onFetch({ request: new Request(url), respondWith: passthrough });
    expect(passthrough).not.toHaveBeenCalled();
  }
});
