import {
  memo,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
} from "react";
import { useQuery, type QueryClient } from "@tanstack/react-query";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
} from "@tanstack/react-table";
import { useVirtualizer } from "@tanstack/react-virtual";
import {
  ArrowDown,
  ArrowUp,
  Check,
  Clock,
  FileArchive,
  FileDown,
  FolderInput,
  Link,
  Magnet,
  Pause,
  SearchCheck,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import { initI18n } from "../../i18n";
import {
  formatAbsolute,
  formatBytes,
  formatEta,
  formatPercent,
  formatRate,
  formatRatio,
  formatWhen,
} from "../../lib/format";
import {
  selectTask,
  useTasks,
  type SidebarFilter,
  type Task,
} from "../../store/useTasks";
import grid from "../../locales/en/grid.json";
import { CardContents } from "./TaskCardList";

initI18n().addResourceBundle("en", "grid", grid, true, true);
const missing = "—";
const pageSize = 500;
const mobileQuery = "(max-width: 639px)";
const rowHeights = { comfortable: 32, compact: 26, cards: 160 } as const;
const widths = [
  36, 44, 320, 90, 130, 110, 90, 90, 80, 100, 70, 90, 200, 140, 140,
];
export const DEFAULT_COLUMN_ORDER = [
  "select",
  "queuePos",
  "name",
  "size",
  "progress",
  "status",
  "dlSpeed",
  "ulSpeed",
  "eta",
  "peers",
  "ratio",
  "uploaded",
  "destination",
  "addedOn",
  "completedOn",
] as const;
type ColumnId = (typeof DEFAULT_COLUMN_ORDER)[number];
export const STATUS_ORDINAL: Record<Task["state"], number> = {
  downloading: 0,
  seeding: 1,
  checking: 2,
  extracting: 3,
  moving: 4,
  queued: 5,
  paused: 6,
  completed: 7,
  error: 8,
  removed: 9,
};
const states = {
  downloading: ["--accent", ArrowDown],
  seeding: ["--ok", ArrowUp],
  checking: ["--warn", SearchCheck],
  extracting: ["--warn", FileArchive],
  moving: ["--warn", FolderInput],
  queued: ["--fg-muted", Clock],
  paused: ["--fg-muted", Pause],
  completed: ["--ok", Check],
  error: ["--error", TriangleAlert],
  removed: ["--fg-muted", Trash2],
} as const;
const rightAligned = new Set<ColumnId>([
  "queuePos",
  "size",
  "dlSpeed",
  "ulSpeed",
  "eta",
  "peers",
  "ratio",
  "uploaded",
]);

export interface TaskGridProps {
  filter: SidebarFilter;
  category?: string;
  tag?: string;
}

export function useTaskIds({ filter, category, tag }: TaskGridProps) {
  const query = useQuery({
    queryKey: ["tasks", filter, category, tag],
    queryFn: async ({ signal }) => {
      const tasks: Task[] = [];
      let cursor: string | undefined;
      let total = 0;
      do {
        const { data, error } = await api.GET("/tasks", {
          signal,
          params: {
            query: { state: filter, category, tag, limit: pageSize, cursor },
          },
        });
        if (!data) throw new Error(error?.detail ?? "Task request failed");
        if (cursor === undefined) total = data.total;
        tasks.push(...(data.items ?? []));
        cursor = data.next_cursor ?? undefined;
      } while (cursor !== undefined);
      // Publish a complete page chain, never a partial list after an error.
      useTasks.getState().hydrate(tasks);
      return { ids: tasks.map((task) => task.id), total };
    },
  });
  return {
    ids: query.data?.ids ?? emptyIds,
    total: query.data?.total ?? 0,
    isLoading: query.isLoading,
    error: query.error,
    refetch: query.refetch,
  };
}
const emptyIds: string[] = [];
export function invalidateTaskList(qc: QueryClient): Promise<void> {
  return qc.invalidateQueries({ queryKey: ["tasks"] });
}

export function TaskStatus({ task }: { task: Task }) {
  const { t } = useTranslation("grid");
  const [token, Icon] = states[task.state as keyof typeof states];
  return (
    <span
      title={task.error_message ?? undefined}
      className="inline-flex items-center gap-1 whitespace-nowrap"
    >
      <span
        aria-hidden="true"
        style={{
          background: `var(${token})`,
          width: 6,
          height: 6,
          borderRadius: "50%",
        }}
      />
      <Icon aria-hidden="true" size={14} style={{ color: `var(${token})` }} />
      {t(`states.${task.state}`)}
      {task.state === "error" && task.error_code ? `: ${task.error_code}` : ""}
    </span>
  );
}
export function TaskProgress({ task }: { task: Task }) {
  const { t, i18n } = useTranslation("grid");
  const percent = formatPercent(task.progress, i18n.language);
  const indeterminate = ["checking", "extracting", "moving"].includes(
    task.state,
  );
  const striped = indeterminate || task.state === "paused";
  const token =
    task.state === "error"
      ? "--error"
      : task.state === "seeding"
        ? "--ok"
        : task.state === "paused"
          ? "--fg-muted"
          : "--accent";
  return (
    <div
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={task.progress * 100}
      aria-valuetext={t("progressValue", {
        percent,
        completed: formatBytes(task.completed_bytes, i18n.language),
        total: formatBytes(task.total_bytes, i18n.language),
      })}
      style={{
        position: "relative",
        height: 16,
        background: "var(--progress-track)",
        overflow: "hidden",
      }}
    >
      <div
        className={
          indeterminate ? "animate-pulse motion-reduce:animate-none" : undefined
        }
        style={{
          position: "absolute",
          inset: 0,
          width: indeterminate ? "100%" : percentWidth(task.progress),
          backgroundColor: `var(${token})`,
          backgroundImage: striped
            ? "repeating-linear-gradient(135deg, transparent 0 4px, #ffffff40 4px 8px)"
            : undefined,
        }}
      />
      <span
        style={{
          position: "absolute",
          inset: 0,
          textAlign: "center",
          mixBlendMode: "difference",
          color: "white",
          lineHeight: "16px",
        }}
      >
        {task.progress ? percent : missing}
      </span>
    </div>
  );
}
function percentWidth(value: number) {
  return `${value * 100}%`;
}

function value(task: Task, id: ColumnId): string | number | null {
  switch (id) {
    case "select":
      return null;
    case "queuePos":
      return task.queue_position;
    case "name":
      return task.name;
    case "size":
      return task.total_bytes;
    case "progress":
      return task.progress;
    case "status":
      return STATUS_ORDINAL[task.state];
    case "dlSpeed":
      return task.download_rate;
    case "ulSpeed":
      return task.upload_rate;
    case "eta":
      return task.eta_seconds;
    case "peers":
      return task.connected_seeders;
    case "ratio":
      return task.ratio;
    case "uploaded":
      return task.uploaded_bytes;
    case "destination":
      return task.destination;
    case "addedOn":
      return Date.parse(task.added_at);
    case "completedOn":
      return task.completed_at ? Date.parse(task.completed_at) : null;
  }
}
function Cell({ task, id }: { task: Task; id: ColumnId }) {
  const { t, i18n } = useTranslation("grid");
  const locale = i18n.language;
  if (id === "select") return null;
  if (id === "status") return <TaskStatus task={task} />;
  if (id === "progress") return <TaskProgress task={task} />;
  if (id === "eta" && task.eta_seconds === null) return formatEta(null, locale);
  if (!value(task, id) && id !== "peers") return missing;
  switch (id) {
    case "name": {
      const Icon =
        task.source_kind === "magnet"
          ? Magnet
          : task.source_kind === "torrent"
            ? FileDown
            : Link;
      return (
        <span title={task.name} className="flex items-center gap-1">
          <Icon aria-label={task.source_kind} size={14} className="shrink-0" />
          <span className="truncate">{task.name}</span>
        </span>
      );
    }
    case "queuePos":
      return new Intl.NumberFormat(locale).format(task.queue_position!);
    case "size":
      return formatBytes(task.total_bytes, locale);
    case "dlSpeed":
      return formatRate(task.download_rate, locale);
    case "ulSpeed":
      return formatRate(task.upload_rate, locale);
    case "eta":
      return formatEta(task.eta_seconds, locale);
    case "peers":
      return task.total_peers ? (
        <span title={t("knownPeers", { count: task.total_peers })}>
          {new Intl.NumberFormat(locale).format(task.connected_seeders)} /{" "}
          {new Intl.NumberFormat(locale).format(task.connected_leechers)}
        </span>
      ) : (
        missing
      );
    case "ratio":
      return formatRatio(task.ratio, locale);
    case "uploaded":
      return formatBytes(task.uploaded_bytes, locale);
    case "destination":
      return (
        <span title={task.destination} className="flex min-w-0">
          <span className="truncate">
            {task.destination.slice(0, Math.ceil(task.destination.length / 2))}
          </span>
          <span className="shrink-0">
            {task.destination.slice(Math.ceil(task.destination.length / 2))}
          </span>
        </span>
      );
    case "addedOn":
    case "completedOn": {
      const date = id === "addedOn" ? task.added_at : task.completed_at!;
      return (
        <span title={formatAbsolute(date, locale)}>
          {formatWhen(date, new Date(), locale)}
        </span>
      );
    }
  }
}
export const columns: ColumnDef<Task>[] = DEFAULT_COLUMN_ORDER.map(
  (id, index) => ({
    id,
    size: widths[index],
    accessorFn: (task) => value(task, id),
    enableSorting: id !== "select",
    sortDescFirst: false,
    header: () => initI18n().t(`grid:headers.${id}`),
    cell: ({ row }) => <Cell task={row.original} id={id} />,
    sortingFn: (a, b) => {
      const left = value(a.original, id),
        right = value(b.original, id);
      if (typeof left === "string" && typeof right === "string")
        return new Intl.Collator(initI18n().language).compare(left, right);
      if (left === right) return 0;
      return ((left ?? Infinity) as number) - ((right ?? Infinity) as number);
    },
  }),
);

function useLayout() {
  const [layout, setLayout] = useState(() => ({
    mobile: window.matchMedia(mobileQuery).matches,
    compact: document.documentElement.dataset.density === "compact",
  }));
  useEffect(() => {
    const media = window.matchMedia(mobileQuery);
    const update = () =>
      setLayout({
        mobile: media.matches,
        compact: document.documentElement.dataset.density === "compact",
      });
    const observer = new MutationObserver(update);
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-density"],
    });
    media.addEventListener("change", update);
    return () => {
      observer.disconnect();
      media.removeEventListener("change", update);
    };
  }, []);
  return layout;
}

// Both entry points share one viewport, including sorting, focus and selection.
export function GridSurface({
  ids,
  total,
  layout: forcedLayout,
}: {
  ids: string[];
  total: number;
  layout?: "cards";
}) {
  const { t } = useTranslation("grid");
  const { mobile, compact } = useLayout();
  const cards = forcedLayout === "cards" || mobile;
  const rowHeight = cards
    ? rowHeights.cards
    : compact
      ? rowHeights.compact
      : rowHeights.comfortable;
  const scroll = useRef<HTMLDivElement>(null);
  const header = useRef<HTMLDivElement>(null);
  const [sorting, setSorting] = useState<SortingState>([]);
  // Table identities change only when membership or a sort key changes, not on every delta.
  const [sortRevision, setSortRevision] = useState(0);
  useEffect(
    () =>
      useTasks.subscribe((next, previous) => {
        const sort = sorting[0]?.id as ColumnId | undefined;
        if (
          sort &&
          ids.some((id) => {
            const a = next.tasks.get(id),
              b = previous.tasks.get(id);
            return a && b && value(a, sort) !== value(b, sort);
          })
        )
          setSortRevision((revision) => revision + 1);
      }),
    [ids, sorting],
  );
  const data = useMemo(() => {
    // sortRevision invalidates only the table's ordering snapshot; rows subscribe independently.
    return {
      revision: sortRevision,
      rows: ids.flatMap((id) => {
        const task = useTasks.getState().tasks.get(id);
        return task ? [task] : [];
      }),
    };
  }, [ids, sortRevision]);
  const table = useReactTable({
    data: data.rows,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getRowId: (task) => task.id,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    columnResizeMode: "onChange",
    enableMultiSort: false,
  });
  const rows = table.getRowModel().rows;
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroll.current,
    estimateSize: () => rowHeight,
    overscan: 10,
  });
  useLayoutEffect(() => {
    virtualizer.measure();
  }, [rowHeight, virtualizer]);
  const [focused, setFocused] = useState<string>();
  const focusCell = useRef<number | null>(null);
  const pendingFocus = useRef(false);
  const anchor = useRef<string | undefined>(undefined);
  const rowElements = useRef(new Map<string, HTMLDivElement>());
  const visible = virtualizer.getVirtualItems();
  const active = visible.some((item) => rows[item.index].id === focused)
    ? focused
    : rows[visible[0]?.index]?.id;
  useLayoutEffect(() => {
    if (!pendingFocus.current || !focused) return;
    const element = rowElements.current.get(focused);
    if (!element) return;
    const target =
      focusCell.current === null
        ? element
        : (element.children[focusCell.current] as HTMLElement);
    target?.focus({ preventScroll: true });
    pendingFocus.current = false;
  });
  function select(
    id: string,
    event: Pick<MouseEvent, "shiftKey" | "ctrlKey" | "metaKey">,
  ) {
    const state = useTasks.getState();
    if (event.shiftKey && anchor.current) {
      const start = rows.findIndex((row) => row.id === anchor.current),
        end = rows.findIndex((row) => row.id === id);
      if (start >= 0) {
        state.setSelection(
          rows
            .slice(Math.min(start, end), Math.max(start, end) + 1)
            .map((row) => row.id),
        );
        return;
      }
    }
    anchor.current = id;
    if (!event.ctrlKey && !event.metaKey) {
      state.setSelection([id]);
      return;
    }
    const next = new Set(state.selection);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    state.setSelection(next);
  }
  function keyDown(event: KeyboardEvent) {
    const target = event.target as HTMLElement;
    if (
      target.tagName === "INPUT" ||
      target.tagName === "TEXTAREA" ||
      target.isContentEditable ||
      target.closest('[role="dialog"]')
    )
      return;
    const state = useTasks.getState();
    const index = rows.findIndex((row) => row.id === active);
    const command = event.ctrlKey || event.metaKey;
    const page = Math.max(
      1,
      Math.floor((scroll.current?.clientHeight ?? rowHeight) / rowHeight),
    );
    let next = index;
    switch (event.key) {
      case "ArrowUp":
        next--;
        break;
      case "ArrowDown":
        next++;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = rows.length - 1;
        break;
      case "PageUp":
        next -= page;
        break;
      case "PageDown":
        next += page;
        break;
      case " ":
        if (active) {
          if (event.shiftKey)
            state.setSelection(new Set([...state.selection, active]));
          else
            select(active, { shiftKey: false, ctrlKey: true, metaKey: false });
        }
        break;
      case "a":
      case "A":
        if (!command) return;
        state.setSelection(rows.map((row) => row.id));
        break;
      case "Escape":
        state.clearSelection();
        break;
      default:
        return;
    }
    event.preventDefault();
    if (
      !rows.length ||
      (next === index && !["Home", "End"].includes(event.key))
    )
      return;
    next = Math.max(0, Math.min(rows.length - 1, next));
    focusCell.current =
      command && event.key === "Home"
        ? 0
        : command && event.key === "End"
          ? cards
            ? 0
            : columns.length - 1
          : null;
    pendingFocus.current = true;
    setFocused(rows[next].id);
    virtualizer.scrollToIndex(next, { align: "auto" });
  }
  const selected = useTasks((state) => state.selection);
  const selectedCount = rows.filter((row) => selected.has(row.id)).length;
  const tableWidth = table.getTotalSize();
  return (
    <div
      style={{
        height: "100%",
        minWidth: 0,
        overflow: "hidden",
        position: "relative",
      }}
    >
      {!cards && (
        <div
          ref={header}
          role="row"
          aria-rowindex={1}
          style={{
            display: "flex",
            position: "absolute",
            top: 0,
            zIndex: 2,
            width: tableWidth,
            height: rowHeight,
            background: "var(--bg-elevated)",
          }}
        >
          {table.getHeaderGroups()[0].headers.map((item, index) => (
            <span
              key={item.id}
              role="columnheader"
              aria-colindex={index + 1}
              aria-sort={
                item.column.getIsSorted() === "asc"
                  ? "ascending"
                  : item.column.getIsSorted() === "desc"
                    ? "descending"
                    : "none"
              }
              style={{
                ...cellStyle(item.id as ColumnId, item.getSize()),
                cursor: "pointer",
              }}
              onClick={item.column.getToggleSortingHandler()}
            >
              {item.id === "select" ? (
                <input
                  type="checkbox"
                  tabIndex={-1}
                  aria-label={t("headers.select")}
                  checked={rows.length > 0 && selectedCount === rows.length}
                  ref={(node) => {
                    if (node)
                      node.indeterminate =
                        selectedCount > 0 && selectedCount < rows.length;
                  }}
                  onChange={() => {
                    if (selectedCount === rows.length)
                      useTasks.getState().clearSelection();
                    else
                      useTasks
                        .getState()
                        .setSelection(rows.map((row) => row.id));
                  }}
                />
              ) : (
                flexRender(item.column.columnDef.header, item.getContext())
              )}
            </span>
          ))}
        </div>
      )}
      <div
        ref={scroll}
        role="grid"
        aria-label={t("headers.name")}
        aria-rowcount={total}
        aria-colcount={cards ? 1 : columns.length}
        aria-multiselectable="true"
        onKeyDown={keyDown}
        onScroll={(event) => {
          if (header.current)
            header.current.style.transform = `translateX(-${event.currentTarget.scrollLeft}px)`;
        }}
        style={{
          overflow: "auto",
          height: "100%",
          paddingTop: cards ? 0 : rowHeight,
          boxSizing: "border-box",
        }}
      >
        <div
          data-testid="virtual-space"
          style={{
            height: virtualizer.getTotalSize(),
            width: cards ? "100%" : tableWidth,
            minWidth: "100%",
            position: "relative",
          }}
        >
          {visible.map((item) => (
            <LiveRow
              key={rows[item.index].id}
              id={rows[item.index].id}
              index={item.index}
              height={rowHeight}
              start={item.start}
              cards={cards}
              focused={active === rows[item.index].id}
              cell={active === rows[item.index].id ? focusCell.current : null}
              register={(node) => {
                if (node) rowElements.current.set(rows[item.index].id, node);
                else rowElements.current.delete(rows[item.index].id);
              }}
              onFocus={() => setFocused(rows[item.index].id)}
              onClick={(event) => {
                focusCell.current = null;
                setFocused(rows[item.index].id);
                select(rows[item.index].id, event);
                event.currentTarget.focus();
              }}
            />
          ))}
        </div>
      </div>
    </div>
  );
}
function cellStyle(id: ColumnId, width: number): CSSProperties {
  return {
    width,
    flexShrink: 0,
    padding: "0 4px",
    boxSizing: "border-box",
    alignContent: "center",
    textAlign:
      id === "select" ? "center" : rightAligned.has(id) ? "right" : "left",
    overflow: "hidden",
    whiteSpace: "nowrap",
  };
}
const LiveRow = memo(function LiveRow({
  id,
  index,
  height,
  start,
  cards,
  focused,
  cell,
  register,
  onFocus,
  onClick,
}: {
  id: string;
  index: number;
  height: number;
  start: number;
  cards: boolean;
  focused: boolean;
  cell: number | null;
  register: (node: HTMLDivElement | null) => void;
  onFocus: () => void;
  onClick: (event: MouseEvent<HTMLDivElement>) => void;
}) {
  const task = useTasks(selectTask(id));
  const selected = useTasks((state) => state.selection.has(id));
  const { t } = useTranslation("grid");
  if (!task) return null;
  return (
    <div
      ref={register}
      role="row"
      aria-rowindex={index + 2}
      aria-selected={selected}
      tabIndex={focused && cell === null ? 0 : -1}
      onFocus={onFocus}
      onClick={onClick}
      style={{
        display: "flex",
        position: "absolute",
        top: 0,
        transform: `translateY(${start}px)`,
        height,
        width: "100%",
        minWidth: cards ? "min-content" : undefined,
        background: selected ? "var(--progress-track)" : "var(--bg)",
      }}
    >
      {cards ? (
        <div
          role="gridcell"
          aria-colindex={1}
          tabIndex={focused && cell === 0 ? 0 : -1}
          style={{ width: "100%" }}
        >
          <CardContents task={task} selected={selected} />
        </div>
      ) : (
        DEFAULT_COLUMN_ORDER.map((column, c) => (
          <span
            key={column}
            role="gridcell"
            aria-colindex={c + 1}
            tabIndex={focused && cell === c ? 0 : -1}
            style={cellStyle(column, widths[c])}
          >
            {column === "select" ? (
              <input
                type="checkbox"
                tabIndex={-1}
                aria-label={t("selectTask", { name: task.name })}
                checked={selected}
                readOnly
                onClick={(event) => {
                  event.stopPropagation();
                  const next = new Set(useTasks.getState().selection);
                  if (selected) next.delete(id);
                  else next.add(id);
                  useTasks.getState().setSelection(next);
                }}
              />
            ) : (
              <Cell task={task} id={column} />
            )}
          </span>
        ))
      )}
    </div>
  );
});

export function TaskGrid(props: TaskGridProps) {
  const { t } = useTranslation("grid");
  const query = useTaskIds(props);
  if (query.isLoading) return <p role="status">{t("loading")}</p>;
  if (query.error)
    return (
      <p role="alert">
        {t("loadError")}{" "}
        <button onClick={() => void query.refetch()}>{t("retry")}</button>
      </p>
    );
  if (!query.ids.length)
    return (
      <p>
        {t(
          props.filter === "all" &&
            props.category === undefined &&
            props.tag === undefined
            ? "empty"
            : "emptyFilter",
        )}
      </p>
    );
  return <GridSurface ids={query.ids} total={query.total} />;
}
