import {
  memo,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
} from "react";
import {
  useQuery,
  useQueryClient,
  type QueryClient,
} from "@tanstack/react-query";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type Row,
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
  Globe,
  Magnet,
  Pause,
  SearchCheck,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { api, apiUrl } from "../../api/client";
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
import { useTasks, type SidebarFilter, type Task } from "../../store/useTasks";
import strings from "../../locales/en/grid.json";
import { TaskCardList } from "./TaskCardList";

initI18n().addResourceBundle("en", "grid", strings);

export interface TaskGridProps {
  filter: SidebarFilter;
  category?: string;
  tag?: string;
  /** Controlled by the preference owner; the grid does not persist it. */
  density?: "comfortable" | "compact";
}
const listKey = ["tasks"];
const pageLimit = 500;
const emptyIds: string[] = [];
const missing = "—";
const mobileQuery = "(max-width: 639px)";
const comfortableHeight = 32;
const compactHeight = 26;
const mobileHeight = 160;

export function invalidateTaskList(qc: QueryClient): Promise<void> {
  return qc.invalidateQueries({ queryKey: listKey });
}

export function useTaskIds(p: TaskGridProps): {
  ids: string[];
  total: number;
  isLoading: boolean;
  error: Error | null;
} {
  const query = useQuery({
    queryKey: [...listKey, p.filter, p.category, p.tag],
    queryFn: async ({ signal }) => {
      const items: Task[] = [];
      let cursor: string | undefined;
      let total = 0;
      do {
        const { data, error } = await api.GET("/tasks", {
          baseUrl: apiUrl(""),
          signal,
          params: {
            query: {
              state: p.filter,
              category: p.category,
              tag: p.tag,
              limit: pageLimit,
              cursor,
            },
          },
        });
        if (error || !data) throw new Error(strings.loadError);
        if (!cursor) total = data.total;
        items.push(...(data.items ?? []));
        cursor = data.next_cursor ?? undefined;
      } while (cursor);
      // Hydrate only a complete page chain, never an aborted filter request.
      useTasks.getState().hydrate(items);
      return { ids: items.map((task) => task.id), total };
    },
  });
  return {
    ids: query.data?.ids ?? emptyIds,
    total: query.data?.total ?? 0,
    isLoading: query.isPending,
    error: query.error,
  };
}

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
const statusIcons: Record<Task["state"], typeof ArrowDown> = {
  downloading: ArrowDown,
  seeding: ArrowUp,
  checking: SearchCheck,
  extracting: FileArchive,
  moving: FolderInput,
  queued: Clock,
  paused: Pause,
  completed: Check,
  error: TriangleAlert,
  removed: Trash2,
};
const statusTokens: Record<Task["state"], string> = {
  downloading: "--accent",
  seeding: "--ok",
  checking: "--warn",
  extracting: "--warn",
  moving: "--warn",
  queued: "--fg-muted",
  paused: "--fg-muted",
  completed: "--ok",
  error: "--error",
  removed: "--fg-muted",
};
const widths = [
  36, 44, 320, 90, 130, 110, 90, 90, 80, 100, 70, 90, 200, 140, 140,
];
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
const sourceFields: Record<Exclude<ColumnId, "select">, keyof Task> = {
  queuePos: "queue_position",
  name: "name",
  size: "total_bytes",
  progress: "progress",
  status: "state",
  dlSpeed: "download_rate",
  ulSpeed: "upload_rate",
  eta: "eta_seconds",
  peers: "connected_seeders",
  ratio: "ratio",
  uploaded: "uploaded_bytes",
  destination: "destination",
  addedOn: "added_at",
  completedOn: "completed_at",
};

export function TaskStatus({ task }: { task: Task }) {
  const { t } = useTranslation("grid");
  const Icon = statusIcons[task.state];
  return (
    <span
      title={task.error_message ?? undefined}
      style={{ display: "inline-flex", alignItems: "center", gap: 4 }}
    >
      <span
        aria-hidden="true"
        style={{
          width: 6,
          height: 6,
          borderRadius: "50%",
          background: `var(${statusTokens[task.state]})`,
        }}
      />
      <Icon aria-hidden="true" size={14} />
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
  return (
    <span
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={task.progress * 100}
      aria-valuetext={t("progressText", {
        percent,
        completed: formatBytes(task.completed_bytes, i18n.language),
        total: formatBytes(task.total_bytes, i18n.language),
      })}
      style={{
        display: "block",
        position: "relative",
        height: 16,
        background: "var(--progress-track)",
        lineHeight: "16px",
        fontSize: 12,
        overflow: "hidden",
        textAlign: "center",
      }}
    >
      <span
        className={
          indeterminate
            ? "grid-stripes grid-indeterminate"
            : task.state === "paused"
              ? "grid-stripes"
              : undefined
        }
        aria-hidden="true"
        style={{
          position: "absolute",
          inset: 0,
          width: indeterminate ? "100%" : `${task.progress * 100}%`,
          backgroundColor: `var(${statusTokens[task.state]})`,
        }}
      />
      <span style={{ position: "relative", mixBlendMode: "difference" }}>
        {task.progress ? percent : missing}
      </span>
    </span>
  );
}

function TaskCell({ task, id }: { task: Task; id: ColumnId }) {
  const { t, i18n } = useTranslation("grid");
  const locale = i18n.language;
  switch (id) {
    case "select":
      return null;
    case "name": {
      const Icon =
        task.source_kind === "magnet"
          ? Magnet
          : task.source_kind === "torrent"
            ? FileDown
            : Globe;
      return (
        <span
          title={task.name}
          style={{ display: "flex", gap: 4, minWidth: 0 }}
        >
          <Icon size={14} aria-hidden="true" style={{ flexShrink: 0 }} />
          <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
            {task.name || missing}
          </span>
        </span>
      );
    }
    case "progress":
      return <TaskProgress task={task} />;
    case "status":
      return <TaskStatus task={task} />;
    case "size":
      return formatBytes(task.total_bytes, locale);
    case "dlSpeed":
      return formatRate(task.download_rate, locale);
    case "ulSpeed":
      return formatRate(task.upload_rate, locale);
    case "eta":
      return task.eta_seconds === 0
        ? missing
        : formatEta(task.eta_seconds, locale);
    case "ratio":
      return task.ratio ? formatRatio(task.ratio, locale) : missing;
    case "uploaded":
      return formatBytes(task.uploaded_bytes, locale);
    case "peers":
      return task.total_peers ? (
        <span title={t("knownPeers", { count: task.total_peers })}>
          {new Intl.NumberFormat(locale).format(task.connected_seeders)} /{" "}
          {new Intl.NumberFormat(locale).format(task.connected_leechers)}
        </span>
      ) : (
        missing
      );
    case "queuePos":
      return task.queue_position
        ? new Intl.NumberFormat(locale).format(task.queue_position)
        : missing;
    case "destination": {
      const path = task.destination;
      return (
        <span title={path} style={{ display: "flex", minWidth: 0 }}>
          <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
            {path.slice(0, Math.ceil(path.length / 2)) || missing}
          </span>
          <span style={{ flexShrink: 0 }}>
            {path.slice(Math.ceil(path.length / 2))}
          </span>
        </span>
      );
    }
    case "addedOn":
    case "completedOn": {
      const value = id === "addedOn" ? task.added_at : task.completed_at;
      return value ? (
        <span title={formatAbsolute(value, locale)}>
          {formatWhen(value, new Date(), locale)}
        </span>
      ) : (
        missing
      );
    }
  }
}

export const columns: ColumnDef<Task>[] = DEFAULT_COLUMN_ORDER.map(
  (id, index) => ({
    id,
    size: widths[index],
    enableSorting: id !== "select",
    sortDescFirst: false,
    accessorFn: (task) => {
      if (id === "select") return null;
      if (id === "status") return STATUS_ORDINAL[task.state];
      const value = task[sourceFields[id]];
      if (id === "addedOn" || id === "completedOn")
        return value ? Date.parse(String(value)) : null;
      return value;
    },
    header: () => initI18n().t(`headers.${id}`, { ns: "grid" }),
    cell: ({ row }) => <TaskCell task={row.original} id={id} />,
    sortingFn: (a, b, columnId) => {
      const left = a.getValue<string | number | null>(columnId);
      const right = b.getValue<string | number | null>(columnId);
      if (left === right) return 0;
      if (left === null) return 1;
      if (right === null) return -1;
      return typeof left === "string" && typeof right === "string"
        ? new Intl.Collator(initI18n().language).compare(left, right)
        : Number(left) - Number(right);
    },
  }),
);

function useRowHeight(density: TaskGridProps["density"]) {
  const [mobile, setMobile] = useState(
    () => window.matchMedia(mobileQuery).matches,
  );
  useEffect(() => {
    const media = window.matchMedia(mobileQuery);
    const update = () => setMobile(media.matches);
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);
  if (mobile) return mobileHeight;
  return density === "compact" ? compactHeight : comfortableHeight;
}

function SelectionBox({ ids }: { ids: string[] }) {
  const { t } = useTranslation("grid");
  const selected = useTasks(
    (state) => ids.filter((id) => state.selection.has(id)).length,
  );
  const ref = useRef<HTMLInputElement>(null);
  useLayoutEffect(() => {
    if (ref.current)
      ref.current.indeterminate = selected > 0 && selected < ids.length;
  }, [selected, ids.length]);
  return (
    <input
      ref={ref}
      type="checkbox"
      tabIndex={-1}
      aria-label={t("headers.select")}
      checked={ids.length > 0 && selected === ids.length}
      onChange={() =>
        useTasks.getState().setSelection(selected === ids.length ? [] : ids)
      }
    />
  );
}

const LiveRow = memo(function LiveRow({
  row,
  index,
  start,
  height,
  focused,
  focusCell,
  onSelect,
  register,
}: {
  row: Row<Task>;
  index: number;
  start: number;
  height: number;
  focused: boolean;
  focusCell: number | null;
  onSelect: (id: string, event: MouseEvent) => void;
  register: (id: string, node: HTMLDivElement | null) => void;
}) {
  const task = useTasks((state) => state.tasks.get(row.id));
  const selected = useTasks((state) => state.selection.has(row.id));
  const { t } = useTranslation("grid");
  if (!task) return null;
  const mobile = height === mobileHeight;
  return (
    <div
      ref={(node) => register(row.id, node)}
      role="row"
      aria-rowindex={index + 2}
      aria-selected={selected}
      tabIndex={focused && focusCell === null ? 0 : -1}
      data-task-id={row.id}
      onClick={(event) => onSelect(row.id, event)}
      style={{
        position: "absolute",
        top: 0,
        transform: `translateY(${start}px)`,
        height,
        width: "100%",
        display: "flex",
        background: selected ? "var(--accent)" : "var(--bg)",
        color: selected ? "var(--accent-fg)" : undefined,
      }}
    >
      {mobile ? (
        <TaskCardList ids={[row.id]} total={1} />
      ) : (
        row.getVisibleCells().map((cell, columnIndex) => {
          const id = cell.column.id as ColumnId;
          const context = cell.getContext();
          return (
            <span
              role="gridcell"
              aria-colindex={columnIndex + 1}
              key={id}
              tabIndex={focused && focusCell === columnIndex ? 0 : -1}
              style={{
                flex: `0 0 ${cell.column.getSize()}px`,
                minWidth: 0,
                padding: "0 4px",
                boxSizing: "border-box",
                whiteSpace: "nowrap",
                overflow: "hidden",
                textAlign:
                  id === "select"
                    ? "center"
                    : rightAligned.has(id)
                      ? "right"
                      : "left",
                alignContent: "center",
                position:
                  id === "select" || id === "name" ? "sticky" : undefined,
                left:
                  id === "select" ? 0 : id === "name" ? widths[0] : undefined,
                zIndex: id === "select" || id === "name" ? 1 : undefined,
                background: "inherit",
              }}
            >
              {id === "select" ? (
                <input
                  type="checkbox"
                  tabIndex={-1}
                  aria-label={t("selectTask", { name: task.name })}
                  checked={selected}
                  readOnly
                />
              ) : (
                flexRender(cell.column.columnDef.cell, {
                  ...context,
                  row: { ...row, original: task },
                })
              )}
            </span>
          );
        })
      )}
    </div>
  );
});

export function TaskGrid(props: TaskGridProps) {
  const { ids, total, isLoading, error } = useTaskIds(props);
  const queryClient = useQueryClient();
  const headerId = useId();
  const bodyId = useId();
  const { t } = useTranslation("grid");
  const height = useRowHeight(props.density);
  const mobile = height === mobileHeight;
  const [sorting, setSorting] = useState<SortingState>([]);
  const [sortRevision, setSortRevision] = useState(0);
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const [focusCell, setFocusCell] = useState<number | null>(null);
  const anchor = useRef<string | null>(null);
  const scroll = useRef<HTMLDivElement>(null);
  const header = useRef<HTMLDivElement>(null);
  const nodes = useRef(new Map<string, HTMLDivElement>());
  const pendingFocus = useRef(false);
  // Live cells subscribe individually. Only sort-key changes rebuild the table model.
  useEffect(
    () =>
      useTasks.subscribe((next, previous) => {
        const key = sorting[0]?.id as Exclude<ColumnId, "select"> | undefined;
        if (
          key &&
          ids.some(
            (id) =>
              next.tasks.get(id)?.[sourceFields[key]] !==
              previous.tasks.get(id)?.[sourceFields[key]],
          )
        )
          setSortRevision((revision) => revision + 1);
      }),
    [ids, sorting],
  );
  const data = useMemo(() => {
    void sortRevision;
    return ids.flatMap((id) => {
      const task = useTasks.getState().tasks.get(id);
      return task ? [task] : [];
    });
  }, [ids, sortRevision]);
  const table = useReactTable({
    data,
    columns,
    getRowId: (task) => task.id,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    columnResizeMode: "onChange",
  });
  const rows = table.getRowModel().rows;
  const orderedIds = useMemo(() => rows.map((row) => row.id), [rows]);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroll.current,
    estimateSize: () => height,
    overscan: 10,
  });
  useLayoutEffect(() => {
    virtualizer.measure();
  }, [height, virtualizer]);
  const visible = virtualizer.getVirtualItems();
  const focused =
    focusedId && orderedIds.includes(focusedId) ? focusedId : orderedIds[0];
  const rovingId = visible.some((item) => rows[item.index].id === focused)
    ? focused
    : rows[visible[0]?.index]?.id;
  const focusNode = (node: HTMLDivElement) => {
    if (focusCell === null || mobile) node.focus({ preventScroll: true });
    else
      node
        .querySelectorAll<HTMLElement>('[role="gridcell"]')
        [focusCell]?.focus({ preventScroll: true });
  };
  const select = (id: string, event: MouseEvent) => {
    const state = useTasks.getState();
    if (
      event.shiftKey &&
      anchor.current &&
      orderedIds.includes(anchor.current)
    ) {
      const ends = [
        orderedIds.indexOf(anchor.current),
        orderedIds.indexOf(id),
      ].sort((a, b) => a - b);
      state.setSelection(orderedIds.slice(ends[0], ends[1] + 1));
    } else if (event.ctrlKey || event.metaKey) {
      const next = new Set(state.selection);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      state.setSelection(next);
      anchor.current = id;
    } else {
      state.setSelection([id]);
      anchor.current = id;
    }
    setFocusedId(id);
    setFocusCell(null);
    nodes.current.get(id)?.focus({ preventScroll: true });
  };
  const onKeyDown = (event: KeyboardEvent) => {
    const target = event.target as HTMLElement;
    if (
      target.tagName === "INPUT" ||
      target.tagName === "TEXTAREA" ||
      target.isContentEditable
    )
      return;
    if (!rows.length) return;
    const current = Math.max(0, orderedIds.indexOf(rovingId));
    let next = current;
    const command = event.ctrlKey || event.metaKey;
    const state = useTasks.getState();
    const viewportRows = Math.max(
      1,
      Math.floor((scroll.current?.clientHeight ?? height) / height),
    );
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
        next -= viewportRows;
        break;
      case "PageDown":
        next += viewportRows;
        break;
      case " ": {
        const selected = new Set(state.selection);
        if (!event.shiftKey && selected.has(rovingId))
          selected.delete(rovingId);
        else selected.add(rovingId);
        state.setSelection(selected);
        anchor.current = rovingId;
        event.preventDefault();
        return;
      }
      case "a":
      case "A":
        if (!command) return;
        state.setSelection(orderedIds);
        event.preventDefault();
        return;
      case "Escape":
        state.clearSelection();
        event.preventDefault();
        return;
      default:
        return;
    }
    event.preventDefault();
    next = Math.max(0, Math.min(rows.length - 1, next));
    setFocusedId(orderedIds[next]);
    setFocusCell(
      command && (event.key === "Home" || event.key === "End") && !mobile
        ? event.key === "Home"
          ? 0
          : DEFAULT_COLUMN_ORDER.length - 1
        : null,
    );
    pendingFocus.current = true;
    virtualizer.scrollToIndex(next, { align: "auto" });
  };
  useLayoutEffect(() => {
    if (!pendingFocus.current) return;
    const node = nodes.current.get(focused);
    if (!node) return;
    focusNode(node);
    pendingFocus.current = false;
  });
  const headerStyle: CSSProperties = {
    display: "flex",
    width: table.getTotalSize(),
    height: comfortableHeight,
  };
  return (
    <section
      style={{
        height: "100%",
        minWidth: 0,
        display: "flex",
        flexDirection: "column",
        overflow: "hidden",
      }}
    >
      <style>{`.grid-stripes { background-image: repeating-linear-gradient(135deg, transparent 0 6px, #ffffff44 6px 12px); } .grid-indeterminate { animation: grid-stripes 1s linear infinite; } @keyframes grid-stripes { to { background-position: 17px 0; } } @media (prefers-reduced-motion: reduce) { .grid-indeterminate { animation: none; } } [data-task-id]:focus-visible, [role=gridcell]:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: -2px; }`}</style>
      {!mobile && (
        <div
          style={{
            overflow: "hidden",
            flexShrink: 0,
            position: "sticky",
            top: 0,
          }}
        >
          <div
            id={headerId}
            ref={header}
            role="row"
            aria-rowindex={1}
            style={headerStyle}
          >
            {table.getHeaderGroups()[0].headers.map((item, index) => (
              <span
                role="columnheader"
                aria-colindex={index + 1}
                aria-sort={
                  item.column.getIsSorted() === "asc"
                    ? "ascending"
                    : item.column.getIsSorted() === "desc"
                      ? "descending"
                      : "none"
                }
                key={item.id}
                onClick={item.column.getToggleSortingHandler()}
                style={{
                  flex: `0 0 ${item.getSize()}px`,
                  background: "var(--bg)",
                  zIndex:
                    item.id === "select" || item.id === "name" ? 1 : undefined,
                  transform:
                    item.id === "select"
                      ? "translateX(var(--grid-scroll-left, 0px))"
                      : item.id === "name"
                        ? `translateX(max(0px, calc(var(--grid-scroll-left, 0px) - ${widths[1]}px)))`
                        : undefined,
                  textAlign: rightAligned.has(item.id as ColumnId)
                    ? "right"
                    : "left",
                }}
              >
                {item.id === "select" ? (
                  <SelectionBox ids={orderedIds} />
                ) : (
                  flexRender(item.column.columnDef.header, item.getContext())
                )}
              </span>
            ))}
          </div>
        </div>
      )}
      <div
        ref={scroll}
        role="grid"
        aria-label={t("headers.name")}
        aria-rowcount={total}
        aria-owns={mobile ? undefined : `${headerId} ${bodyId}`}
        aria-colcount={mobile ? 1 : DEFAULT_COLUMN_ORDER.length}
        aria-multiselectable="true"
        onKeyDown={onKeyDown}
        onScroll={(event) => {
          event.currentTarget.parentElement?.style.setProperty(
            "--grid-scroll-left",
            `${event.currentTarget.scrollLeft}px`,
          );
          if (header.current)
            header.current.style.transform = `translateX(-${event.currentTarget.scrollLeft}px)`;
        }}
        style={{ overflow: "auto", flex: 1, minHeight: 0, minWidth: 0 }}
      >
        {error && (
          <p role="alert">
            {t("loadError")}{" "}
            <button onClick={() => void invalidateTaskList(queryClient)}>
              {t("retry")}
            </button>
          </p>
        )}
        {isLoading ? (
          <p role="status">{t("loading")}</p>
        ) : rows.length === 0 ? (
          <p>
            {t(
              props.filter === "all" &&
                props.category === undefined &&
                props.tag === undefined
                ? "empty"
                : "filteredEmpty",
            )}
          </p>
        ) : null}
        <div
          id={bodyId}
          data-testid="virtual-rows"
          style={{
            height: virtualizer.getTotalSize(),
            position: "relative",
            width: mobile ? "100%" : table.getTotalSize(),
            minWidth: "100%",
          }}
        >
          {visible.map((item) => (
            <LiveRow
              key={rows[item.index].id}
              row={rows[item.index]}
              index={item.index}
              start={item.start}
              height={height}
              focused={rows[item.index].id === rovingId}
              focusCell={focusCell}
              onSelect={select}
              register={(id, node) => {
                if (node) nodes.current.set(id, node);
                else nodes.current.delete(id);
              }}
            />
          ))}
        </div>
      </div>
    </section>
  );
}
