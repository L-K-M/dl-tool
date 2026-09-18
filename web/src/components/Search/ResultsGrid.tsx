import { createContext, useContext, useMemo, useRef, type JSX } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import type { components } from "../../api/schema";
import { formatBytes, formatInteger, formatWhen } from "../../lib/format";
import { useTasks } from "../../store/useTasks";
import { Checkbox } from "../ui/checkbox";

/** Mirrors the generated SearchResultDTO: metadata and the opaque res_ id only.
 *  download_url, magnet_uri and details_url are server-only acquisition data and
 *  have no field here (doc 05 §9.2, doc 07 §5 rule 6). */
export type SearchResultView = components["schemas"]["SearchResultDTO"];

/** Per-row state the screen owns and the grid only renders: which res_ ids
 *  already became tasks (resolved → ✓ link, unselectable), which ids the
 *  last submit failed for (failed → marked, still selectable for retry),
 *  which indexers cannot report swarm counts (seeders/leechers greyed),
 *  and which rows are mid-flash after a successful grab. Passed by context
 *  so the grid's props stay exactly the task contract's five. */
export interface ResultRowState {
  resolved: ReadonlyMap<string, string | null>;
  failed: ReadonlySet<string>;
  seedersUnknown: ReadonlySet<string>;
  flashing: ReadonlySet<string>;
}

const emptyRowState: ResultRowState = {
  resolved: new Map(),
  failed: new Set(),
  seedersUnknown: new Set(),
  flashing: new Set(),
};

export const ResultRowStateContext =
  createContext<ResultRowState>(emptyRowState);

/** Columns in this order, matching doc 09 §7: checkbox, Name, Size, Seeders,
 *  Leechers, Age, Indexer, and the per-row download action. Default sort is
 *  seeders descending. A null seeder or leecher count renders the em dash
 *  "—", never 0 and never -1. */
export const RESULT_COLUMN_ORDER = [
  "select",
  "title",
  "size",
  "seeders",
  "leechers",
  "age",
  "indexer",
  "actions",
] as const;

const ROW_HEIGHT = 32;
const OVERSCAN = 10;
const GRID_COLUMNS =
  "grid grid-cols-[2rem_minmax(0,1fr)_6rem_5rem_5rem_6rem_9rem_3.5rem] items-center gap-x-2 px-2";

export function ResultsGrid(props: {
  results: SearchResultView[];
  total: number;
  selected: Set<string>;
  onSelectedChange: (next: Set<string>) => void;
  onDownload: (r: SearchResultView, mode: "immediate" | "choose") => void;
}): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const { resolved, failed, seedersUnknown, flashing } = useContext(
    ResultRowStateContext,
  );
  const scroll = useRef<HTMLDivElement>(null);

  // The server already answers -seeders; the client sort keeps that order
  // stable across incremental polls, nulls last (doc 09 §7 default sort).
  const rows = useMemo(
    () =>
      [...props.results].sort((a, b) => (b.seeders ?? -1) - (a.seeders ?? -1)),
    [props.results],
  );

  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroll.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: OVERSCAN,
  });

  const selectable = useMemo(
    () => rows.filter((r) => !resolved.has(r.id)),
    [rows, resolved],
  );
  const allSelected =
    selectable.length > 0 && selectable.every((r) => props.selected.has(r.id));
  const someSelected = selectable.some((r) => props.selected.has(r.id));

  const toggleAll = (on: boolean) => {
    const next = new Set(props.selected);
    for (const r of selectable) {
      if (on) next.add(r.id);
      else next.delete(r.id);
    }
    props.onSelectedChange(next);
  };
  const toggleRow = (id: string, on: boolean) => {
    const next = new Set(props.selected);
    if (on) next.add(id);
    else next.delete(id);
    props.onSelectedChange(next);
  };

  const headers = [
    t("search.colName"),
    t("search.colSize"),
    t("search.colSeeders"),
    t("search.colLeechers"),
    t("search.colAge"),
    t("search.colIndexer"),
    t("search.colActions"),
  ];

  return (
    <div
      role="grid"
      aria-label={t("search.resultsRegion")}
      // aria-rowcount is the job's total, not the rendered row count
      // (doc 09 §10.4): the virtualiser mounts only the visible window.
      aria-rowcount={props.total}
      className="flex min-h-0 flex-1 flex-col text-sm"
    >
      <div
        role="row"
        aria-rowindex={1}
        className={`${GRID_COLUMNS} border-b py-1 text-muted-foreground`}
      >
        <span role="columnheader">
          <Checkbox
            aria-label={t("search.selectAll")}
            checked={
              allSelected ? true : someSelected ? "indeterminate" : false
            }
            onCheckedChange={(checked) => toggleAll(checked === true)}
          />
        </span>
        {headers.map((label) => (
          <span key={label} role="columnheader" className="truncate">
            {label}
          </span>
        ))}
      </div>
      <div ref={scroll} className="min-h-0 flex-1 overflow-auto">
        <div
          style={{ height: virtualizer.getTotalSize(), position: "relative" }}
        >
          {virtualizer.getVirtualItems().map((item) => {
            const r = rows[item.index];
            const done = resolved.has(r.id);
            const countsUnknown = seedersUnknown.has(r.indexer_id);
            const countClass = countsUnknown
              ? "text-right tabular-nums text-muted-foreground"
              : "text-right tabular-nums";
            return (
              <div
                key={r.id}
                role="row"
                aria-rowindex={item.index + 2}
                aria-selected={props.selected.has(r.id)}
                className={`${GRID_COLUMNS} border-b ${flashing.has(r.id) ? "bg-accent" : ""}`}
                style={{
                  position: "absolute",
                  top: 0,
                  left: 0,
                  width: "100%",
                  height: item.size,
                  transform: `translateY(${item.start}px)`,
                }}
              >
                <span role="gridcell">
                  <Checkbox
                    aria-label={t("search.selectRow", { title: r.title })}
                    checked={props.selected.has(r.id)}
                    disabled={done}
                    onCheckedChange={(checked) =>
                      toggleRow(r.id, checked === true)
                    }
                  />
                </span>
                <span role="gridcell" className="truncate" title={r.title}>
                  {r.title}
                </span>
                <span role="gridcell" className="text-right tabular-nums">
                  {formatBytes(r.size_bytes, locale)}
                </span>
                <span role="gridcell" className={countClass}>
                  {r.seeders === null ? "—" : formatInteger(r.seeders, locale)}
                </span>
                <span role="gridcell" className={countClass}>
                  {r.leechers === null
                    ? "—"
                    : formatInteger(r.leechers, locale)}
                </span>
                <span
                  role="gridcell"
                  className="truncate text-muted-foreground"
                >
                  {r.published_at === null
                    ? "—"
                    : formatWhen(r.published_at, undefined, locale)}
                </span>
                <span
                  role="gridcell"
                  className="truncate"
                  title={r.indexer_name}
                >
                  {r.indexer_name}
                </span>
                <span role="gridcell">
                  {done ? (
                    <Link
                      to="/tasks/all"
                      // No per-task route exists; the link lands on the
                      // task list with the created task selected.
                      onClick={() => {
                        const taskId = resolved.get(r.id);
                        if (taskId) useTasks.getState().setSelection([taskId]);
                      }}
                      aria-label={t("search.downloadedTask", {
                        title: r.title,
                      })}
                      className="inline-block px-1 text-[var(--ok)]"
                    >
                      ✓
                    </Link>
                  ) : (
                    <button
                      type="button"
                      aria-label={t("search.downloadOne", { title: r.title })}
                      title={
                        failed.has(r.id) ? t("search.downloadRetry") : undefined
                      }
                      className={`rounded px-1 hover:bg-accent ${failed.has(r.id) ? "text-destructive" : ""}`}
                      onClick={() => props.onDownload(r, "immediate")}
                    >
                      {failed.has(r.id) ? "⟲" : "⬇"}
                    </button>
                  )}
                </span>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
