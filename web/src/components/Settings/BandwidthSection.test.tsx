import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import { useState } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
} from "vitest";
import { initI18n } from "../../i18n";
import {
  type Brush,
  type Cells,
  clearAll,
  copyMondayToWeekdays,
  fillAll,
  idx,
  invert,
  paintRect,
  ScheduleGrid,
} from "./ScheduleGrid";
import { SettingsScreen, useSettingsDirty } from "./SettingsScreen";

const server = setupServer();
let qc: QueryClient;
let settingsBody: Record<string, unknown>;
let scheduleBody: Record<string, unknown>;
let settingsPatches: Record<string, unknown>[];
let schedulePuts: Record<string, unknown>[];

function freshQueryClient() {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
}

function mount() {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/settings/bandwidth"]}>
        <Routes>
          <Route path="/settings/:section" element={<SettingsScreen />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function allDefault(): Cells {
  return new Array<Brush>(168).fill(1);
}

/** Renders the grid standalone under a controlled wrapper — the same
 *  controlled cycle the section runs — and records every emitted array. */
function mountGrid(initial: Cells, brush: Brush, disabled = false) {
  const changes: Cells[] = [];
  function Wrapper() {
    const [cells, setCells] = useState<Cells>(initial);
    return (
      <ScheduleGrid
        cells={cells}
        brush={brush}
        disabled={disabled}
        onChange={(next) => {
          changes.push(next);
          setCells(next);
        }}
      />
    );
  }
  render(<Wrapper />);
  return changes;
}

function cell(day: string, hour: string, state = "Default speed") {
  return screen.getByRole("gridcell", {
    name: `${day} ${hour}:00 — ${state}`,
  });
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});
beforeEach(() => {
  freshQueryClient();
  settingsBody = {
    download_rate_limit: 0,
    upload_rate_limit: 0,
    alt_download_rate_limit: 5242880,
    alt_upload_rate_limit: 1048576,
  };
  scheduleBody = {
    enabled: true,
    timezone: "Europe/Zurich",
    active_mode: "default",
    cells: allDefault(),
  };
  settingsPatches = [];
  schedulePuts = [];
  useSettingsDirty.setState({ report: null });
  server.use(
    http.get("*/api/v1/settings", () => HttpResponse.json(settingsBody)),
    http.get("*/api/v1/settings/schedule", () =>
      HttpResponse.json(scheduleBody),
    ),
    http.patch("*/api/v1/settings", async ({ request }) => {
      const patch = (await request.json()) as Record<string, unknown>;
      settingsPatches.push(patch);
      settingsBody = { ...settingsBody, ...patch };
      return HttpResponse.json(settingsBody);
    }),
    http.put("*/api/v1/settings/schedule", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      schedulePuts.push(body);
      scheduleBody = { ...scheduleBody, ...body };
      return HttpResponse.json(scheduleBody);
    }),
  );
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());

test("TestBulkHelpersArePure", () => {
  const base = Array.from({ length: 168 }, (_, i) => (i % 3) as Brush);
  const snapshot = base.slice();
  const results = [
    fillAll(base, 2),
    clearAll(base),
    copyMondayToWeekdays(base),
    invert(base),
    paintRect(base, idx(0, 0), idx(1, 1), 0),
  ];
  for (const next of results) {
    expect(next).toHaveLength(168);
    expect(next).not.toBe(base);
  }
  expect(base).toEqual(snapshot);

  expect(fillAll(base, 0).every((cell) => cell === 0)).toBe(true);
  expect(clearAll(base).every((cell) => cell === 1)).toBe(true);

  const mondayAlt = base.slice();
  for (let hour = 0; hour < 24; hour++) mondayAlt[idx(0, hour)] = 2;
  const copied = copyMondayToWeekdays(mondayAlt);
  for (let day = 1; day <= 4; day++)
    for (let hour = 0; hour < 24; hour++)
      expect(copied[idx(day, hour)]).toBe(2);
  // Saturday and Sunday keep their own values.
  for (let hour = 0; hour < 24; hour++) {
    expect(copied[idx(5, hour)]).toBe(mondayAlt[idx(5, hour)]);
    expect(copied[idx(6, hour)]).toBe(mondayAlt[idx(6, hour)]);
  }
});

test("TestDragPaintsRange", () => {
  const changes = mountGrid(allDefault(), 2);
  fireEvent.pointerDown(cell("Monday", "00"));
  fireEvent.pointerOver(cell("Monday", "01"));
  fireEvent.pointerOver(cell("Monday", "02"));
  fireEvent.pointerOver(cell("Monday", "03"));
  fireEvent.pointerUp(window);
  const last = changes.at(-1);
  expect(last).toBeDefined();
  // Only the four dragged cells carry the brush.
  expect(last!.slice(0, 4)).toEqual([2, 2, 2, 2]);
  expect(last!.filter((value) => value === 2)).toHaveLength(4);
});

test("TestShiftDragPaintsRectangle", () => {
  const changes = mountGrid(allDefault(), 0);
  fireEvent.pointerDown(cell("Monday", "00"));
  // Shift held: the move paints the rectangle from the anchor, not a line.
  fireEvent.pointerOver(cell("Wednesday", "02"), { shiftKey: true });
  fireEvent.pointerUp(window);
  const last = changes.at(-1)!;
  // Days Mon..Wed × hours 00..02 = 9 cells.
  expect(last.filter((value) => value === 0)).toHaveLength(9);
  expect(last[idx(1, 1)]).toBe(0);
  expect(last[idx(2, 2)]).toBe(0);
  expect(last[idx(3, 0)]).toBe(1);
  expect(last[idx(0, 3)]).toBe(1);
});

test("TestHeaderPaintsDayAndHour", () => {
  const changes = mountGrid(allDefault(), 0);
  fireEvent.click(screen.getByRole("button", { name: "Tue" }));
  let last = changes.at(-1)!;
  expect(last.slice(idx(1, 0), idx(1, 0) + 24)).toEqual(new Array(24).fill(0));
  expect(last.filter((value) => value === 0)).toHaveLength(24);

  fireEvent.click(screen.getByRole("button", { name: "05" }));
  last = changes.at(-1)!;
  for (let day = 0; day < 7; day++) expect(last[idx(day, 5)]).toBe(0);
  // Tuesday 05:00 was already painted by the row click — the sets overlap on
  // exactly that cell.
  expect(last.filter((value) => value === 0)).toHaveLength(24 + 7 - 1);
});

test("TestKeyboardMapPaintsAndRoves", () => {
  const changes = mountGrid(allDefault(), 2);
  const cells = () => screen.getAllByRole("gridcell");
  const roved = () => cells().filter((el) => el.tabIndex === 0);
  expect(cells()).toHaveLength(168);
  expect(roved()).toHaveLength(1);

  const first = cells()[0]!;
  first.focus();
  fireEvent.keyDown(first, { key: "ArrowRight" });
  expect(document.activeElement).toBe(cells()[1]);
  expect(roved()).toHaveLength(1);
  expect(cells()[1]!.tabIndex).toBe(0);

  fireEvent.keyDown(cells()[1]!, { key: " " });
  expect(changes.at(-1)![1]).toBe(2);
  expect(changes.at(-1)!.filter((value) => value === 2)).toHaveLength(1);

  // Space dropped the anchor on the focused cell; Shift+ArrowDown extends a
  // Monday-01 → Tuesday-01 rectangle.
  fireEvent.keyDown(cells()[1]!, { key: "ArrowDown", shiftKey: true });
  const last = changes.at(-1)!;
  expect(last[idx(0, 1)]).toBe(2);
  expect(last[idx(1, 1)]).toBe(2);
  expect(document.activeElement).toBe(cells()[idx(1, 1)]);
  expect(roved()).toHaveLength(1);
});

test("TestInvertLeavesAlternativeUntouched", () => {
  const cells = Array.from({ length: 168 }, (_, i) => (i % 3) as Brush);
  const next = invert(cells);
  for (let i = 0; i < 168; i++) {
    if (cells[i] === 2) expect(next[i]).toBe(2);
    else if (cells[i] === 0) expect(next[i]).toBe(1);
    else expect(next[i]).toBe(0);
  }
  expect(next.filter((value) => value === 2)).toHaveLength(56);
});

test("TestSaveSendsBothBodies", async () => {
  mount();
  const download = (await screen.findByLabelText(
    "Download limit",
  )) as HTMLInputElement;
  expect(download.value).toBe("0");

  fireEvent.change(download, { target: { value: "1048576" } });
  fireEvent.change(screen.getByLabelText("Download"), {
    target: { value: "262144" },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));

  await waitFor(() =>
    expect(settingsPatches).toEqual([
      { download_rate_limit: 1048576, alt_download_rate_limit: 262144 },
    ]),
  );
  await waitFor(() => expect(schedulePuts).toHaveLength(1));
  const put = schedulePuts[0]!;
  expect((put.cells as number[]).length).toBe(168);
  expect(put.enabled).toBe(true);
  expect(put.timezone).toBe("Europe/Zurich");
  expect(put.active_mode).toBe("default");
  // Bytes per second on the wire, verbatim — no KB/s value is sent anywhere.
  for (const patch of settingsPatches)
    for (const value of Object.values(patch)) {
      expect(Number.isInteger(value)).toBe(true);
      expect(value).not.toBe(1024);
    }
  await waitFor(() =>
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull(),
  );
});

test("TestImmediatelyDisablesGridAndSendsEnabledFalse", async () => {
  scheduleBody = { ...scheduleBody, enabled: false };
  mount();
  const grid = await screen.findByRole("grid");
  expect(grid.getAttribute("aria-disabled")).toBe("true");
  expect(
    (screen.getByRole("radio", { name: "Immediately" }) as HTMLInputElement)
      .checked,
  ).toBe(true);

  // A gesture on the inert grid paints nothing.
  fireEvent.pointerDown(cell("Monday", "00"));
  fireEvent.pointerOver(cell("Monday", "01"));
  expect(document.querySelector('[aria-live="polite"]')?.textContent).toBe("");

  fireEvent.change(await screen.findByLabelText("Upload limit"), {
    target: { value: "42" },
  });
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(schedulePuts).toHaveLength(1));
  expect(schedulePuts[0]!.enabled).toBe(false);
  expect(schedulePuts[0]!.cells).toEqual(allDefault());
  expect(settingsPatches).toEqual([{ upload_rate_limit: 42 }]);
});
