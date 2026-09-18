import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";
import { DEFAULT_COLUMN_ORDER } from "../components/TaskGrid/ColumnsMenu";

const server = setupServer();

// Every PUT body the debounced writer sent, and the responder each pending
// GET waits on — a test controls exactly when a hydrate snapshot lands.
const putBodies: Record<string, unknown>[] = [];
const getResponders: ((doc: Record<string, unknown> | unknown[]) => void)[] =
  [];
let getCalls = 0;

async function freshStore() {
  vi.resetModules();
  return await import("./useUiPrefs");
}

/** Resolves the next pending GET /prefs with doc. The fetch reaches the
 *  handler a few microtasks after hydrate() returns, so the responder is
 *  awaited rather than assumed. */
async function answerGet(doc: Record<string, unknown> | unknown[]) {
  await vi.waitFor(() =>
    expect(getResponders.length, "a pending GET /prefs").toBeGreaterThan(0),
  );
  getResponders.shift()!(doc);
}

/** Runs hydrate() to completion against doc, the seed every write-path
 *  test needs now that PUTs are gated on an applied server snapshot. */
async function hydrateNow(
  useUiPrefs: Awaited<ReturnType<typeof freshStore>>["useUiPrefs"],
  doc: Record<string, unknown> = { version: 1 },
) {
  const pending = useUiPrefs.getState().hydrate();
  await answerGet(doc);
  await pending;
}

beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  putBodies.length = 0;
  getResponders.length = 0;
  getCalls = 0;
  server.use(
    http.get(
      "*/api/v1/prefs",
      () =>
        new Promise<Response>((resolve) => {
          getCalls += 1;
          getResponders.push((doc) => resolve(HttpResponse.json(doc)));
        }),
    ),
    http.put("*/api/v1/prefs", async ({ request }) => {
      putBodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({});
    }),
  );
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  server.resetHandlers();
});
afterAll(() => server.close());

test("TestDefaultsBeforeHydrate", async () => {
  const { useUiPrefs, defaultPrefs } = await freshStore();
  const state = useUiPrefs.getState();
  for (const key of Object.keys(defaultPrefs) as (keyof typeof defaultPrefs)[])
    expect(state[key]).toEqual(defaultPrefs[key]);
  expect(state.grid.order).toEqual([...DEFAULT_COLUMN_ORDER]);
});

test("TestHydrateAppliesServerValues", async () => {
  const { useUiPrefs } = await freshStore();
  const pending = useUiPrefs.getState().hydrate();
  await answerGet({ version: 1, sidebarWidth: 400, theme: "dark" });
  await pending;
  const state = useUiPrefs.getState();
  expect(state.sidebarWidth).toBe(400);
  expect(state.theme).toBe("dark");
  // Members absent from the document fall back to their defaults.
  expect(state.detailHeight).toBe(260);
});

test("TestHydrateShapeChecksLikeLoadInitial", async () => {
  const { useUiPrefs, defaultPrefs } = await freshStore();
  // A version the SPA does not own is ignored whole, the rule loadInitial
  // applied to the persisted copy.
  let pending = useUiPrefs.getState().hydrate();
  await answerGet({ version: 2, grid: { order: [] }, savedSearches: [1] });
  await pending;
  expect(useUiPrefs.getState().grid).toEqual(defaultPrefs.grid);
  expect(useUiPrefs.getState().savedSearches).toBeUndefined();
  // A wrong-shaped member inside a v1 document falls back individually.
  pending = useUiPrefs.getState().hydrate();
  await answerGet({ version: 1, grid: { order: "broken" } });
  await pending;
  expect(useUiPrefs.getState().grid.order).toEqual(defaultPrefs.grid.order);
  // A non-object document is not a preference document.
  pending = useUiPrefs.getState().hydrate();
  await answerGet([1, 2]);
  await pending;
  expect(useUiPrefs.getState().grid.order).toEqual(defaultPrefs.grid.order);
});

test("TestHydrateFailedGetKeepsDefaults", async () => {
  server.use(http.get("*/api/v1/prefs", () => HttpResponse.error()));
  const { useUiPrefs, defaultPrefs } = await freshStore();
  await useUiPrefs.getState().hydrate();
  expect(useUiPrefs.getState().grid).toEqual(defaultPrefs.grid);
  expect(useUiPrefs.getState().theme).toBe("system");
  // The document's state is unknown, so the client stays unhydrated.
  expect(useUiPrefs.getState().hydrated).toBe(false);
});

test("TestUnknownMembersLandVerbatimAndRoundTrip", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const pending = useUiPrefs.getState().hydrate();
  await answerGet({
    version: 1,
    grid: { order: ["size", "name"], future: { nested: true } },
    savedSearches: { keep: [1, 2] },
  });
  await pending;
  // Unknown top-level members land in state verbatim; a nested member inside
  // a declared one survives the shape-checking merge.
  expect(useUiPrefs.getState().savedSearches).toEqual({ keep: [1, 2] });
  expect(
    (useUiPrefs.getState().grid as Record<string, unknown>).future,
  ).toEqual({ nested: true });
  const state = useUiPrefs.getState();
  state.patch({
    grid: { ...state.grid, sizing: { name: 500 } },
    sidebarWidth: 300,
  });
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  const written = putBodies[0]!;
  expect(written.savedSearches).toEqual({ keep: [1, 2] });
  expect((written.grid as Record<string, unknown>).future).toEqual({
    nested: true,
  });
  expect((written.grid as Record<string, unknown>).order).toEqual([
    "size",
    "name",
  ]);
  // Deep merge keeps the stored member's untouched keys.
  expect((written.grid as Record<string, unknown>).sizing).toEqual({
    name: 500,
    destination: 240,
  });
  expect(written.sidebarWidth).toBe(300);
});

test("TestLocalEditSurvivesInFlightHydrate", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const pending = useUiPrefs.getState().hydrate();
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  // The GET resolves while the patch is still dirty: the local edit is kept
  // and the untouched members still land.
  await answerGet({ version: 1, sidebarWidth: 100, detailHeight: 999 });
  await pending;
  expect(useUiPrefs.getState().sidebarWidth).toBe(280);
  expect(useUiPrefs.getState().detailHeight).toBe(999);
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.sidebarWidth).toBe(280);
});

test("TestStaleHydrateSnapshotDiscardedAndRetried", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs, { version: 1 });
  // A second hydrate (re-auth) goes in flight, then the debounced PUT
  // completes while that GET is still unanswered.
  const pending = useUiPrefs.getState().hydrate();
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  // The snapshot predates the PUT: it is discarded and the GET re-issued once.
  await answerGet({ version: 1, sidebarWidth: 100 });
  await vi.waitFor(() => expect(getCalls).toBe(3));
  // The retried snapshot post-dates the write; it lands without reverting
  // the member the PUT carried.
  await answerGet({ version: 1, sidebarWidth: 280 });
  await pending;
  expect(useUiPrefs.getState().sidebarWidth).toBe(280);
  expect(getCalls).toBe(3);
});

test("TestNoWriteBeforeHydrate", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  const pending = useUiPrefs.getState().hydrate();
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  // A PUT built from pre-hydrate defaults would erase the stored document,
  // so the armed write re-attempts hydration instead of firing.
  await vi.advanceTimersByTimeAsync(2000);
  expect(putBodies).toHaveLength(0);
  await answerGet({ version: 1, theme: "dark", detailHeight: 999 });
  await pending;
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  // The flushed body carries the local patch over the hydrated members —
  // nothing the server held is lost.
  expect(putBodies[0]!.sidebarWidth).toBe(280);
  expect(putBodies[0]!.theme).toBe("dark");
  expect(putBodies[0]!.detailHeight).toBe(999);
});

test("TestFailedPutRetriedOnceWithDirtyMembers", async () => {
  vi.useFakeTimers();
  let putCalls = 0;
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      putCalls += 1;
      // The write and its one retry both fail; later writes succeed.
      if (putCalls <= 2) return HttpResponse.error();
      putBodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({});
    }),
  );
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs, { version: 1, sidebarWidth: 220 });
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putCalls).toBe(1);
  // One bounded retry carries the still-dirty members.
  await vi.advanceTimersByTimeAsync(2600);
  expect(putCalls).toBe(2);
  expect(putBodies).toHaveLength(0);
  // The failed retry does not arm another: the count stays put.
  await vi.advanceTimersByTimeAsync(5000);
  expect(putCalls).toBe(2);
  // The members stayed dirty, so the next patch's write carries them.
  useUiPrefs.getState().patch({ detailHeight: 111 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putCalls).toBe(3);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.sidebarWidth).toBe(280);
  expect(putBodies[0]!.detailHeight).toBe(111);
});

test("TestResetClearsSessionDocument", async () => {
  vi.useFakeTimers();
  const { useUiPrefs, defaultPrefs } = await freshStore();
  await hydrateNow(useUiPrefs, {
    version: 1,
    theme: "dark",
    savedSearches: { keep: [1] },
  });
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  useUiPrefs.getState().reset();
  const state = useUiPrefs.getState();
  // Every member — declared and unknown — returns to defaults, and the
  // armed write is cancelled so nothing of this account is flushed later.
  expect(state.theme).toBe("system");
  expect(state.sidebarWidth).toBe(defaultPrefs.sidebarWidth);
  expect(state.savedSearches).toBeUndefined();
  expect(state.hydrated).toBe(false);
  await vi.advanceTimersByTimeAsync(1000);
  expect(putBodies).toHaveLength(0);
});

test("TestInFlightHydrateDiscardedAfterReset", async () => {
  vi.useFakeTimers();
  const { useUiPrefs, defaultPrefs } = await freshStore();
  const pending = useUiPrefs.getState().hydrate();
  // The session ends while the GET is on the wire; its snapshot must not
  // merge into the fresh post-reset state.
  useUiPrefs.getState().reset();
  await answerGet({ version: 1, theme: "dark", savedSearches: { keep: [1] } });
  await pending;
  const state = useUiPrefs.getState();
  expect(state.theme).toBe("system");
  expect(state.sidebarWidth).toBe(defaultPrefs.sidebarWidth);
  expect(state.savedSearches).toBeUndefined();
  expect(state.hydrated).toBe(false);
});

test("TestSecondPutWaitsForInFlight", async () => {
  vi.useFakeTimers();
  const putResponders: (() => void)[] = [];
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      putBodies.push((await request.json()) as Record<string, unknown>);
      // Each PUT holds until the test resolves it, so the second write
      // provably cannot overtake the first on the wire.
      return new Promise<Response>((resolve) => {
        putResponders.push(() => resolve(HttpResponse.json({})));
      });
    }),
  );
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs, { version: 1 });
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  // A second patch while the first PUT is in flight does not issue a
  // concurrent write — the member stays dirty for the settle-time flush.
  useUiPrefs.getState().patch({ detailHeight: 111 });
  await vi.advanceTimersByTimeAsync(1200);
  expect(putBodies).toHaveLength(1);
  putResponders.shift()!();
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(2);
  expect(putBodies[1]!.sidebarWidth).toBe(280);
  expect(putBodies[1]!.detailHeight).toBe(111);
});

test("TestFailedPutRetryWaitsForInFlight", async () => {
  vi.useFakeTimers();
  let putCalls = 0;
  const putResponders: (() => void)[] = [];
  server.use(
    http.put("*/api/v1/prefs", async ({ request }) => {
      putCalls += 1;
      if (putCalls === 1) return HttpResponse.error();
      putBodies.push((await request.json()) as Record<string, unknown>);
      return new Promise<Response>((resolve) => {
        putResponders.push(() => resolve(HttpResponse.json({})));
      });
    }),
  );
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs, { version: 1 });
  useUiPrefs.getState().patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putCalls).toBe(1);
  // The failed write arms a retry at +2 s; a second patch's PUT is on the
  // wire when it fires, and the retry must wait behind it like any write.
  useUiPrefs.getState().patch({ detailHeight: 111 });
  await vi.advanceTimersByTimeAsync(600);
  expect(putCalls).toBe(2);
  // The retry timer fires mid-flight but no concurrent PUT is issued.
  await vi.advanceTimersByTimeAsync(2600);
  expect(putCalls).toBe(2);
  // The in-flight write's body already carried every dirty member, so the
  // settle arms nothing further — no duplicate, no out-of-order retry.
  putResponders.shift()!();
  await vi.advanceTimersByTimeAsync(600);
  expect(putCalls).toBe(2);
  expect(putBodies.at(-1)!.sidebarWidth).toBe(280);
  expect(putBodies.at(-1)!.detailHeight).toBe(111);
});

test("TestNoWriteDuringDrag", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs);
  const state = useUiPrefs.getState();
  state.setDragging(true);
  state.patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(1000);
  expect(putBodies).toHaveLength(0);
  state.setDragging(false);
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.sidebarWidth).toBe(280);
});

test("TestSingleDebouncedWrite", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs);
  const { patch } = useUiPrefs.getState();
  patch({ sidebarWidth: 260 });
  patch({ sidebarWidth: 270 });
  patch({ detailHeight: 100 });
  await vi.advanceTimersByTimeAsync(499);
  expect(putBodies).toHaveLength(0);
  await vi.advanceTimersByTimeAsync(1);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.sidebarWidth).toBe(270);
  expect(putBodies[0]!.detailHeight).toBe(100);
});

test("TestPendingWriteDoesNotFireDuringDrag", async () => {
  vi.useFakeTimers();
  const { useUiPrefs } = await freshStore();
  await hydrateNow(useUiPrefs);
  const state = useUiPrefs.getState();
  // A write armed before the gesture starts must not land mid-gesture.
  state.patch({ sidebarWidth: 280 });
  await vi.advanceTimersByTimeAsync(100);
  state.setDragging(true);
  await vi.advanceTimersByTimeAsync(1000);
  expect(putBodies).toHaveLength(0);
  state.setDragging(false);
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.sidebarWidth).toBe(280);
});

test("TestResetGridRestoresColumnOrder", async () => {
  vi.useFakeTimers();
  const { useUiPrefs, defaultPrefs } = await freshStore();
  await hydrateNow(useUiPrefs);
  const state = useUiPrefs.getState();
  state.patch({
    grid: {
      ...state.grid,
      order: [...DEFAULT_COLUMN_ORDER].reverse(),
      sorting: [],
    },
  });
  useUiPrefs.getState().resetGrid();
  expect(useUiPrefs.getState().grid.order).toEqual([...DEFAULT_COLUMN_ORDER]);
  expect(useUiPrefs.getState().grid).toEqual(defaultPrefs.grid);
  await vi.advanceTimersByTimeAsync(600);
  expect(putBodies).toHaveLength(1);
  expect(putBodies[0]!.grid).toEqual(defaultPrefs.grid);
});
