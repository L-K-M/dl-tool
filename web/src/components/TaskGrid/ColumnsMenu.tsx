import { useState, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { ArrowDown, ArrowUp, ChevronDown, Columns3 } from "lucide-react";
import { create } from "zustand";
import type { Column, Table } from "@tanstack/react-table";
import { initI18n } from "../../i18n";
import { useUiPrefs } from "../../store/useUiPrefs";
import type { Task } from "../../store/useTasks";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import gridStrings from "../../locales/en/grid.json";

initI18n().addResourceBundle("en", "grid", gridStrings);

/** Column ids that stay pinned left; never draggable or reorderable. */
export const PINNED_GRID_COLUMNS: ReadonlySet<string> = new Set([
  "select",
  "name",
]);

/**
 * The grid's TanStack Table instance, published by TaskGrid so the toolbar's
 * Columns popover can reach it without a shared parent. Null off the tasks
 * route; Toolbar renders a disabled Columns button then.
 */
export const useGridTable = create<{ table: Table<Task> | null }>(() => ({
  table: null,
}));

/**
 * A column order where pinned columns keep their canonical slots and the
 * movable columns keep `stored`'s relative order. Unknown ids drop out and
 * known ids missing from `stored` append in canonical order.
 */
export function normalizeGridOrder(
  stored: string[],
  all: readonly string[],
): string[] {
  const known = new Set(all);
  const movable: string[] = [];
  const seen = new Set<string>();
  for (const id of stored) {
    if (known.has(id) && !PINNED_GRID_COLUMNS.has(id) && !seen.has(id)) {
      movable.push(id);
      seen.add(id);
    }
  }
  for (const id of all) {
    if (!PINNED_GRID_COLUMNS.has(id) && !seen.has(id)) movable.push(id);
  }
  const order: string[] = [];
  let index = 0;
  for (const id of all)
    order.push(PINNED_GRID_COLUMNS.has(id) ? id : movable[index++]);
  return order;
}

/** Rebuild `order` with pinned ids at their slots and `movable` filling the rest. */
function withPinned(order: string[], movable: string[]): string[] {
  const next: string[] = [];
  let index = 0;
  for (const id of order)
    next.push(PINNED_GRID_COLUMNS.has(id) ? id : movable[index++]);
  return next;
}

/**
 * Drag-and-drop reorder of a movable column onto another movable column's
 * slot. Pinned endpoints keep `order` unchanged.
 */
export function moveGridColumn(
  order: string[],
  activeId: string,
  overId: string,
): string[] {
  if (PINNED_GRID_COLUMNS.has(activeId) || PINNED_GRID_COLUMNS.has(overId))
    return order;
  const movable = order.filter((id) => !PINNED_GRID_COLUMNS.has(id));
  const from = movable.indexOf(activeId);
  const to = movable.indexOf(overId);
  if (from < 0 || to < 0 || from === to) return order;
  const next = [...movable];
  next.splice(to, 0, ...next.splice(from, 1));
  return withPinned(order, next);
}

/**
 * Move a movable column one list position earlier or later; the popover's
 * keyboard alternative to dragging. Pinned columns and boundary moves keep
 * `order` unchanged.
 */
export function shiftGridColumn(
  order: string[],
  id: string,
  delta: -1 | 1,
): string[] {
  if (PINNED_GRID_COLUMNS.has(id)) return order;
  const movable = order.filter((x) => !PINNED_GRID_COLUMNS.has(x));
  const from = movable.indexOf(id);
  const to = from + delta;
  if (from < 0 || to < 0 || to >= movable.length) return order;
  const next = [...movable];
  [next[from], next[to]] = [next[to], next[from]];
  return withPinned(order, next);
}

// The toolbar's icon-only breakpoint (doc 09 section 2.5): label drops out of
// the accessibility tree, so the trigger also carries an aria-label.
const iconLabelClass = "max-[1100px]:hidden";

export function ColumnsMenu({ table }: { table: Table<Task> }): JSX.Element {
  const { t } = useTranslation();
  const { t: gt } = useTranslation("grid");
  const grid = useUiPrefs((state) => state.grid);
  const resetGrid = useUiPrefs((state) => state.resetGrid);
  const [query, setQuery] = useState("");
  const allIds = table.getAllLeafColumns().map((column) => column.id);
  const order = normalizeGridOrder(grid.order, allIds);
  const movable = order.filter((id) => !PINNED_GRID_COLUMNS.has(id));
  const needle = query.trim().toLowerCase();
  const rows = order.flatMap((id) => {
    const column: Column<Task, unknown> | undefined = table.getColumn(id);
    if (!column) return [];
    const label = gt(`headers.${id}`);
    if (needle && !label.toLowerCase().includes(needle)) return [];
    return [{ column, id, label }];
  });
  const move = (id: string, delta: -1 | 1) =>
    table.setColumnOrder(shiftGridColumn(order, id, delta));
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="ghost" size="sm" aria-label={t("shell.columns")}>
          <Columns3 aria-hidden="true" />{" "}
          <span className={iconLabelClass}>{t("shell.columns")}</span>{" "}
          <ChevronDown aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-64">
        <Input
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          aria-label={t("shell.columnsMenu.filter")}
          placeholder={t("shell.columnsMenu.filter")}
          className="h-7"
        />
        <div
          role="group"
          aria-label={t("shell.columns")}
          className="flex max-h-72 flex-col gap-0.5 overflow-y-auto"
        >
          {rows.map(({ column, id, label }) => {
            const index = movable.indexOf(id);
            return (
              <div key={id} className="flex items-center gap-1">
                <label className="flex min-w-0 flex-1 items-center gap-1.5">
                  <Checkbox
                    checked={column.getIsVisible()}
                    disabled={!column.getCanHide()}
                    onCheckedChange={(checked) =>
                      column.toggleVisibility(checked === true)
                    }
                  />
                  <span className="truncate">{label}</span>
                </label>
                <Button
                  variant="ghost"
                  size="icon-xs"
                  disabled={index <= 0}
                  aria-label={t("shell.columnsMenu.moveUp", { name: label })}
                  onClick={() => move(id, -1)}
                >
                  <ArrowUp aria-hidden="true" />
                </Button>
                <Button
                  variant="ghost"
                  size="icon-xs"
                  disabled={index < 0 || index === movable.length - 1}
                  aria-label={t("shell.columnsMenu.moveDown", { name: label })}
                  onClick={() => move(id, 1)}
                >
                  <ArrowDown aria-hidden="true" />
                </Button>
              </div>
            );
          })}
        </div>
        <Button variant="outline" size="sm" onClick={resetGrid}>
          {t("shell.columnsMenu.reset")}
        </Button>
      </PopoverContent>
    </Popover>
  );
}
