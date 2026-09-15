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
  DndContext,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  horizontalListSortingStrategy,
  useSortable,
} from "@dnd-kit/sortable";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type ColumnSizingInfoState,
  type Header,
  type Row,
  type Updater,
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
import { useShallow } from "zustand/react/shallow";
import { api, apiUrl } from "../../api/client";
import { initI18n } from "../../i18n";
import {
  formatAbsolute,
  formatBytes,
  formatEta,
  formatInteger,
  formatPercent,
  formatRate,
  formatRatio,
  formatWhen,
} from "../../lib/format";
import { useTasks, type SidebarFilter, type Task } from "../../store/useTasks";
import { useUiPrefs, type UiPrefs } from "../../store/useUiPrefs";
import { useDebouncedNameFilter, useTaskPending } from "../Shell/Toolbar";
import strings from "../../locales/en/grid.json";
import { TaskCardList } from "./TaskCardList";
import {
  DEFAULT_COLUMN_ORDER,
  PINNED_GRID_COLUMNS,
  moveGridColumn,
  normalizeGridOrder,
  useGridTable,
} from "./ColumnsMenu";

initI18n().addResourceBundle("en", "grid", strings);

// @dnd-kit's published declarations still reference the global JSX namespace
// that React 19's types removed; alias the members they use.
declare global {
  // eslint-disable-next-line @typescript-eslint/no-namespace
  namespace JSX {
    type Element = import("react").JSX.Element;
    type IntrinsicElements = import("react").JSX.IntrinsicElements;
  }
}

/** The shell callbacks this task's doc 09 §3.6 keys dispatch to; wired in App.tsx. */
export interface TaskGridActions {
  requestRemove?: (ids: string[], deleteFiles: boolean) => void;
  focusFilter?: () => void;
  showShortcuts?: () => void;
  /** Enter/F2: select the focused task — which need not be selected — and open its pane. */
  openDetail?: (id: string) => void;
}

export interface TaskGridProps {
  filter: SidebarFilter;
  category?: string;
  tag?: string;
  /** Controlled by the preference owner; the grid does not persist it. */
  density?: "comfortable" | "compact";
  actions?: TaskGridActions;
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

export { DEFAULT_COLUMN_ORDER };
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

/** Status cell: subscribes to only the fields it renders, so sync ticks that
 *  touch unrelated fields leave the mounted status cells undisturbed. */
export function TaskStatus({ taskId }: { taskId: string }) {
  const [state, errorMessage, errorCode] = useTasks(
    useShallow((store) => {
      const task = store.tasks.get(taskId);
      return [task?.state, task?.error_message, task?.error_code];
    }),
  );
  const { t } = useTranslation("grid");
  if (state === undefined) return null;
  const Icon = statusIcons[state];
  return (
    <span
      title={errorMessage ?? undefined}
      style={{ display: "inline-flex", alignItems: "center", gap: 4 }}
    >
      <span
        aria-hidden="true"
        style={{
          width: 6,
          height: 6,
          borderRadius: "50%",
          background: `var(${statusTokens[state]})`,
        }}
      />
      <Icon aria-hidden="true" size={14} />
      {t(`states.${state}`)}
      {state === "error" && errorCode ? `: ${errorCode}` : ""}
    </span>
  );
}

export function TaskProgress({ taskId }: { taskId: string }) {
  const [progress, completedBytes, totalBytes, state] = useTasks(
    useShallow((store) => {
      const task = store.tasks.get(taskId);
      return [
        task?.progress,
        task?.completed_bytes,
        task?.total_bytes,
        task?.state,
      ];
    }),
  );
  const { t, i18n } = useTranslation("grid");
  if (progress === undefined || state === undefined) return null;
  const percent = formatPercent(progress, i18n.language);
  const indeterminate = ["checking", "extracting", "moving"].includes(state);
  return (
    <span
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={progress * 100}
      aria-valuetext={t("progressText", {
        percent,
        completed: formatBytes(completedBytes ?? 0, i18n.language),
        total: formatBytes(totalBytes ?? 0, i18n.language),
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
            : state === "paused"
              ? "grid-stripes"
              : undefined
        }
        aria-hidden="true"
        style={{
          position: "absolute",
          inset: 0,
          width: indeterminate ? "100%" : `${progress * 100}%`,
          backgroundColor: `var(${statusTokens[state]})`,
        }}
      />
      <span style={{ position: "relative", mixBlendMode: "difference" }}>
        {progress ? percent : missing}
      </span>
    </span>
  );
}

/** Fields each column renders; the cell's store subscription selects exactly
 *  these, so a sync tick re-renders only cells whose displayed value moved.
 *  Progress and status delegate to self-subscribing leaf components. */
const cellFields: Record<
  Exclude<ColumnId, "select">,
  readonly (keyof Task)[]
> = {
  name: ["name", "source_kind"],
  progress: [],
  status: [],
  size: ["total_bytes"],
  dlSpeed: ["download_rate"],
  ulSpeed: ["upload_rate"],
  eta: ["eta_seconds"],
  peers: ["connected_seeders", "connected_leechers", "total_peers"],
  ratio: ["ratio"],
  uploaded: ["uploaded_bytes"],
  queuePos: ["queue_position"],
  destination: ["destination"],
  addedOn: ["added_at"],
  completedOn: ["completed_at"],
};

function TaskCell({ taskId, id }: { taskId: string; id: ColumnId }) {
  const values = useTasks(
    useShallow((state) => {
      const task = state.tasks.get(taskId);
      const fields = id === "select" ? [] : cellFields[id];
      return fields.map((field) => task?.[field]);
    }),
  );
  const { t, i18n } = useTranslation("grid");
  const locale = i18n.language;
  switch (id) {
    case "select":
      return null;
    case "name": {
      const [name, sourceKind] = values as [string, Task["source_kind"]];
      const Icon =
        sourceKind === "magnet"
          ? Magnet
          : sourceKind === "torrent"
            ? FileDown
            : Globe;
      return (
        <span title={name} style={{ display: "flex", gap: 4, minWidth: 0 }}>
          <Icon size={14} aria-hidden="true" style={{ flexShrink: 0 }} />
          <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
            {name || missing}
          </span>
        </span>
      );
    }
    case "progress":
      return <TaskProgress taskId={taskId} />;
    case "status":
      return <TaskStatus taskId={taskId} />;
    case "size":
      return formatBytes(values[0] as number | null, locale);
    case "dlSpeed":
      return formatRate(values[0] as number, locale);
    case "ulSpeed":
      return formatRate(values[0] as number, locale);
    case "eta":
      return values[0] === 0
        ? missing
        : formatEta(values[0] as number | null, locale);
    case "ratio":
      return values[0] ? formatRatio(values[0] as number, locale) : missing;
    case "uploaded":
      return formatBytes(values[0] as number | null, locale);
    case "peers": {
      const [seeders, leechers, totalPeers] = values as [
        number,
        number,
        number,
      ];
      return totalPeers ? (
        <span title={t("knownPeers", { count: totalPeers })}>
          {formatInteger(seeders, locale)} / {formatInteger(leechers, locale)}
        </span>
      ) : (
        missing
      );
    }
    case "queuePos":
      return values[0] ? formatInteger(values[0] as number, locale) : missing;
    case "destination": {
      const path = values[0] as string;
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
      const value = values[0] as string | null;
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
    enableHiding: !PINNED_GRID_COLUMNS.has(id),
    enableResizing: id !== "select",
    accessorFn: (task) => {
      if (id === "select") return null;
      if (id === "status") return STATUS_ORDINAL[task.state];
      const value = task[sourceFields[id]];
      if (id === "addedOn" || id === "completedOn")
        return value ? Date.parse(String(value)) : null;
      return value;
    },
    header: () => initI18n().t(`headers.${id}`, { ns: "grid" }),
    cell: ({ row }) => <TaskCell taskId={row.id} id={id} />,
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

const resolveUpdater = <T,>(update: Updater<T>, current: T): T =>
  typeof update === "function" ? (update as (old: T) => T)(current) : update;

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
  // selection keeps its reference across data-only ticks, so the visible-id
  // scan runs on real selection changes, not on every sync.
  const selection = useTasks((state) => state.selection);
  const selected = useMemo(
    () => ids.filter((id) => selection.has(id)).length,
    [selection, ids],
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
  /** Changes when column order or visibility does; the memo check uses it to
   *  re-render rows whose cell list changed while their row object stayed. */
  columnsSignature: string;
}) {
  // The row subscribes to the name alone; per-column cells subscribe to their
  // own fields, so a tick touching other fields skips the row entirely. The
  // name doubles as the removal sentinel: a dropped row unmounts itself.
  const taskName = useTasks((state) => state.tasks.get(row.id)?.name);
  const selected = useTasks((state) => state.selection.has(row.id));
  const pending = useTaskPending(row.id);
  const { t } = useTranslation("grid");
  if (taskName === undefined) return null;
  const mobile = height === mobileHeight;
  return (
    <div
      ref={(node) => register(row.id, node)}
      role="row"
      aria-rowindex={index + 2}
      aria-selected={selected}
      aria-busy={pending || undefined}
      tabIndex={focused && focusCell === null ? 0 : -1}
      data-task-id={row.id}
      className={pending ? "task-pending" : undefined}
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
                flex: `0 0 calc(var(--col-${id}-size) * 1px)`,
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
                  aria-label={t("selectTask", { name: taskName })}
                  checked={selected}
                  readOnly
                />
              ) : (
                flexRender(cell.column.columnDef.cell, context)
              )}
            </span>
          );
        })
      )}
    </div>
  );
});

function GridHeader({
  header,
  index,
  ids,
  middleOffset,
  sortCount,
  suppressClick,
  onAutoFit,
}: {
  header: Header<Task, unknown>;
  index: number;
  ids: string[];
  /** Width of the visible columns rendered between `select` and `name`. */
  middleOffset: number;
  sortCount: number;
  suppressClick: { current: boolean };
  onAutoFit: (id: string) => void;
}) {
  const id = header.column.id as ColumnId;
  const pinned = PINNED_GRID_COLUMNS.has(id);
  const { t: tc } = useTranslation();
  const { t: gt } = useTranslation("grid");
  const { setNodeRef, listeners, transform, transition, isDragging } =
    useSortable({ id: header.id, disabled: pinned });
  const sorted = header.column.getIsSorted();
  return (
    <span
      ref={setNodeRef}
      {...listeners}
      role="columnheader"
      aria-colindex={index + 1}
      aria-sort={
        sorted === "asc"
          ? "ascending"
          : sorted === "desc"
            ? "descending"
            : "none"
      }
      data-column-id={id}
      onClick={(event) => {
        // The pointer-up that ends a column drag also fires a click; it must
        // not toggle the sort.
        if (suppressClick.current) return;
        header.column.getToggleSortingHandler()?.(event);
      }}
      style={{
        flex: `0 0 calc(var(--col-${id}-size) * 1px)`,
        position: "relative",
        display: "inline-flex",
        alignItems: "center",
        gap: 2,
        padding: "0 4px",
        boxSizing: "border-box",
        overflow: "hidden",
        whiteSpace: "nowrap",
        background: "var(--bg)",
        zIndex: isDragging ? 3 : pinned ? 1 : undefined,
        transform: pinned
          ? id === "select"
            ? "translateX(var(--grid-scroll-left, 0px))"
            : `translateX(max(0px, calc(var(--grid-scroll-left, 0px) - ${middleOffset}px)))`
          : transform
            ? `translateX(${transform.x}px)`
            : undefined,
        transition,
        textAlign: rightAligned.has(id) ? "right" : "left",
        cursor: pinned ? undefined : isDragging ? "grabbing" : "grab",
      }}
    >
      {id === "select" ? (
        <SelectionBox ids={ids} />
      ) : (
        <>
          <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
            {flexRender(header.column.columnDef.header, header.getContext())}
          </span>
          {sorted === "asc" ? (
            <ArrowUp aria-hidden="true" size={12} style={{ flexShrink: 0 }} />
          ) : sorted === "desc" ? (
            <ArrowDown aria-hidden="true" size={12} style={{ flexShrink: 0 }} />
          ) : null}
          {sortCount > 1 && sorted ? (
            <span
              aria-hidden="true"
              style={{
                flexShrink: 0,
                width: 14,
                height: 14,
                borderRadius: "50%",
                fontSize: 10,
                lineHeight: "14px",
                textAlign: "center",
                background: "var(--accent)",
                color: "var(--accent-fg)",
              }}
            >
              {header.column.getSortIndex() + 1}
            </span>
          ) : null}
        </>
      )}
      {header.column.getCanResize() ? (
        <span
          role="separator"
          aria-orientation="vertical"
          aria-valuemin={header.column.columnDef.minSize ?? 20}
          aria-valuemax={
            header.column.columnDef.maxSize ?? Number.MAX_SAFE_INTEGER
          }
          aria-valuenow={Math.round(header.column.getSize())}
          // title, not aria-label: a descendant's label leaks into the
          // columnheader's name-from-content; title is not consulted there.
          title={tc("shell.resizeColumn", { name: gt(`headers.${id}`) })}
          tabIndex={0}
          data-resize-handle={id}
          onPointerDown={(event) => event.stopPropagation()}
          onMouseDown={header.getResizeHandler()}
          onTouchStart={header.getResizeHandler()}
          onClick={(event) => event.stopPropagation()}
          onKeyDown={(event) => {
            if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
            event.preventDefault();
            event.stopPropagation();
            const delta = event.key === "ArrowLeft" ? -16 : 16;
            header.getContext().table.setColumnSizing((old) => ({
              ...old,
              // Same bounds the pointer path enforces via columnDef.
              [id]: Math.min(
                Math.max(
                  header.column.getSize() + delta,
                  header.column.columnDef.minSize ?? 20,
                ),
                header.column.columnDef.maxSize ?? Number.MAX_SAFE_INTEGER,
              ),
            }));
          }}
          onDoubleClick={(event) => {
            event.stopPropagation();
            onAutoFit(id);
          }}
          style={{
            position: "absolute",
            top: 0,
            right: 0,
            bottom: 0,
            width: 5,
            cursor: "col-resize",
            zIndex: 2,
          }}
        />
      ) : null}
    </span>
  );
}

export function TaskGrid(props: TaskGridProps) {
  const { ids, total, isLoading, error } = useTaskIds(props);
  const queryClient = useQueryClient();
  const headerId = useId();
  const bodyId = useId();
  const { t } = useTranslation("grid");
  const gridPrefs = useUiPrefs((state) => state.grid);
  const setDragging = useUiPrefs((state) => state.setDragging);
  const height = useRowHeight(props.density ?? gridPrefs.density);
  const mobile = height === mobileHeight;
  const sorting = gridPrefs.sorting;
  const columnOrder = useMemo(
    () => normalizeGridOrder(gridPrefs.order, DEFAULT_COLUMN_ORDER),
    [gridPrefs.order],
  );
  const [columnSizingInfo, setColumnSizingInfo] =
    useState<ColumnSizingInfoState>({
      columnSizingStart: [],
      deltaOffset: null,
      deltaPercentage: null,
      isResizingColumn: false,
      startOffset: null,
      startSize: null,
    });
  const [sortRevision, setSortRevision] = useState(0);
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const [focusCell, setFocusCell] = useState<number | null>(null);
  const anchor = useRef<string | null>(null);
  const scroll = useRef<HTMLDivElement>(null);
  const header = useRef<HTMLDivElement>(null);
  const nodes = useRef(new Map<string, HTMLDivElement>());
  const pendingFocus = useRef(false);
  // Live cells subscribe individually. Only sort-key changes rebuild the table
  // model, and the changed-id set keeps each check proportional to the delta.
  const idSet = useMemo(() => new Set(ids), [ids]);
  useEffect(
    () =>
      // Delta merges mutate the shared task map, so the pre-update value comes
      // from changedFrom, not from the previous state object.
      useTasks.subscribe((next) => {
        const changed = sorting.some(({ id: columnId }) => {
          const field = sourceFields[columnId as Exclude<ColumnId, "select">];
          for (const id of next.changedIds) {
            if (!idSet.has(id)) continue;
            if (
              next.tasks.get(id)?.[field] !== next.changedFrom.get(id)?.[field]
            )
              return true;
          }
          return false;
        });
        if (changed) setSortRevision((revision) => revision + 1);
      }),
    [idSet, sorting],
  );
  const nameFilter = useDebouncedNameFilter();
  const data = useMemo(() => {
    void sortRevision;
    const needle = nameFilter.trim().toLowerCase();
    return ids.flatMap((id) => {
      const task = useTasks.getState().tasks.get(id);
      if (!task) return [];
      if (needle && !task.name.toLowerCase().includes(needle)) return [];
      return [task];
    });
  }, [ids, sortRevision, nameFilter]);
  const patchGrid = (part: Partial<UiPrefs["grid"]>) =>
    useUiPrefs
      .getState()
      .patch({ grid: { ...useUiPrefs.getState().grid, ...part } });
  const table = useReactTable({
    data,
    columns,
    getRowId: (task) => task.id,
    state: {
      sorting,
      columnOrder,
      // Pinned columns stay visible even if a hand-edited document hides them;
      // their popover checkboxes are disabled, so nothing else could recover.
      columnVisibility: {
        ...gridPrefs.visibility,
        select: true,
        name: true,
      },
      columnSizing: gridPrefs.sizing,
      columnSizingInfo,
    },
    onSortingChange: (update) => {
      patchGrid({ sorting: resolveUpdater(update, sorting) });
      // Unsorted ticks update cells, not table snapshots; refresh before sorting.
      setSortRevision((revision) => revision + 1);
    },
    onColumnOrderChange: (update) =>
      patchGrid({
        order: normalizeGridOrder(
          resolveUpdater(update, columnOrder),
          DEFAULT_COLUMN_ORDER,
        ),
      }),
    onColumnVisibilityChange: (update) =>
      patchGrid({ visibility: resolveUpdater(update, gridPrefs.visibility) }),
    onColumnSizingChange: (update) =>
      patchGrid({ sizing: resolveUpdater(update, gridPrefs.sizing) }),
    onColumnSizingInfoChange: setColumnSizingInfo,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    enableColumnResizing: true,
    columnResizeMode: "onChange",
  });
  // The toolbar's Columns popover reaches the table through this store; the
  // shell layout gives TaskGrid and Toolbar no common editable parent.
  useEffect(() => {
    useGridTable.setState({ table });
    return () => useGridTable.setState({ table: null });
  }, [table]);
  // While a resize gesture is live the prefs store must not schedule a write
  // (doc 09 section 3.3); gesture end flushes the accumulated state once.
  useEffect(() => {
    setDragging(columnSizingInfo.isResizingColumn !== false);
  }, [columnSizingInfo.isResizingColumn, setDragging]);
  // Unmounting mid-gesture (e.g. a route change during a drag) must not leave
  // the prefs store in no-write mode for the rest of the session.
  useEffect(() => () => setDragging(false), [setDragging]);
  // No KeyboardSensor: headers carry the click-to-sort handler, so a focusable
  // sortable would start a drag on the same Enter/Space that sorts. Keyboard
  // reordering is the Columns popover's Move buttons (doc 09 section 3.4).
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
  );
  const suppressHeaderClick = useRef(false);
  const endColumnDrag = () => {
    setDragging(false);
    // The pointer-up that ends a drag also fires a click on a header; keep the
    // suppression flag up just long enough to swallow exactly that click.
    setTimeout(() => {
      suppressHeaderClick.current = false;
    }, 0);
  };
  const onColumnDragEnd = (event: DragEndEvent) => {
    endColumnDrag();
    const over = event.over?.id;
    if (over === undefined || event.active.id === over) return;
    const next = moveGridColumn(
      columnOrder,
      String(event.active.id),
      String(over),
    );
    if (next !== columnOrder) table.setColumnOrder(next);
  };
  // Doc 09 section 3.4: double-click on the grab zone auto-fits to content.
  // Cells clip their overflow, so scrollWidth measures the full content.
  const fitColumn = (columnId: string) => {
    const column = table.getColumn(columnId);
    if (!column) return;
    const index = table
      .getVisibleLeafColumns()
      .findIndex((leaf) => leaf.id === columnId);
    let width = 0;
    const headerNode = header.current?.querySelector<HTMLElement>(
      `[data-column-id="${columnId}"]`,
    );
    if (headerNode) width = headerNode.scrollWidth;
    for (const node of nodes.current.values()) {
      const cell =
        node.querySelectorAll<HTMLElement>('[role="gridcell"]')[index];
      if (cell) width = Math.max(width, cell.scrollWidth);
    }
    // No layout engine (or no rendered content): restore the default width.
    // resetSize would delete the key, which a deep-merge patch cannot express.
    if (index < 0 || width <= 0) {
      table.setColumnSizing({
        ...table.getState().columnSizing,
        [columnId]: column.columnDef.size ?? 100,
      });
      return;
    }
    table.setColumnSizing({
      ...table.getState().columnSizing,
      // Content measurement plus cell padding, clamped to the column bounds.
      [columnId]: Math.min(
        Math.max(width + 8, column.columnDef.minSize ?? 20),
        column.columnDef.maxSize ?? Number.MAX_SAFE_INTEGER,
      ),
    });
  };
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
    } else if (
      event.ctrlKey ||
      event.metaKey ||
      (event.target instanceof HTMLInputElement &&
        event.target.type === "checkbox")
    ) {
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
    const command = event.ctrlKey || event.metaKey;
    const state = useTasks.getState();
    // Shell actions and selection clearing do not need visible rows: the name
    // filter can empty the grid while a selection is still live, and Ctrl+F/?/
    // Delete must still work then (doc 09 section 3.6 ownership rules).
    switch (event.key) {
      case "Delete": {
        // Empty-selection removal is a no-op; it neither dispatches nor
        // suppresses the key. Held-key auto-repeat must not re-open the
        // confirmation flow either.
        if (
          event.repeat ||
          !props.actions?.requestRemove ||
          state.selection.size === 0
        )
          return;
        props.actions.requestRemove([...state.selection], event.shiftKey);
        event.preventDefault();
        return;
      }
      case "f":
      case "F":
        if (!command || !props.actions?.focusFilter) return;
        props.actions.focusFilter();
        event.preventDefault();
        return;
      case "?":
        if (!props.actions?.showShortcuts) return;
        props.actions.showShortcuts();
        event.preventDefault();
        return;
      case "Escape":
        // Same no-op rule as Delete: with nothing selected the grid must not
        // swallow a key that a focused dialog may still own.
        if (state.selection.size === 0) return;
        state.clearSelection();
        event.preventDefault();
        return;
      default:
        break;
    }
    if (!rows.length) return;
    const current = Math.max(0, orderedIds.indexOf(rovingId));
    let next = current;
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
      case "Enter":
      case "F2":
        // Doc 09 §3.6: the detail key acts on the focused row, not the
        // selection. With no owner wired the key stays unconsumed.
        if (!props.actions?.openDetail || !rovingId) return;
        props.actions.openDetail(rovingId);
        event.preventDefault();
        return;
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
  const visibleColumns = table.getVisibleLeafColumns();
  const nameIndex = visibleColumns.findIndex((c) => c.id === "name");
  const middleOffset = visibleColumns
    .slice(1, Math.max(1, nameIndex))
    .reduce((sum, column) => sum + column.getSize(), 0);
  const columnsSignature = visibleColumns.map((c) => c.id).join(",");
  // The performant-resize technique of doc 09 section 3.4: widths live in CSS
  // variables on the grid root, so a resize gesture rewrites one style instead
  // of restyling every rendered cell per frame.
  const columnSizeVars: Record<string, number> = {};
  for (const leaf of table.getFlatHeaders()) {
    columnSizeVars[`--col-${leaf.column.id}-size`] = leaf.getSize();
  }
  return (
    <section
      style={
        {
          height: "100%",
          minWidth: 0,
          display: "flex",
          flexDirection: "column",
          overflow: "hidden",
          ...columnSizeVars,
        } as CSSProperties
      }
    >
      <style>{`.grid-stripes { background-image: repeating-linear-gradient(135deg, transparent 0 6px, #ffffff44 6px 12px); } .grid-indeterminate { animation: grid-stripes 1s linear infinite; } @keyframes grid-stripes { to { background-position: 17px 0; } } @media (prefers-reduced-motion: reduce) { .grid-indeterminate { animation: none; } } .task-pending::after { content: ""; position: absolute; inset: 0; border: 1px solid var(--accent); animation: task-shimmer 1s ease-in-out infinite; pointer-events: none; } @keyframes task-shimmer { 50% { opacity: 0.25; } } @media (prefers-reduced-motion: reduce) { .task-pending::after { animation: none; } } [data-task-id]:focus-visible, [role=gridcell]:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: -2px; }`}</style>
      {!mobile && (
        <div
          style={{
            overflow: "hidden",
            flexShrink: 0,
            position: "sticky",
            top: 0,
          }}
        >
          <DndContext
            sensors={sensors}
            collisionDetection={closestCenter}
            onDragStart={() => {
              suppressHeaderClick.current = true;
              setDragging(true);
            }}
            onDragEnd={onColumnDragEnd}
            onDragCancel={endColumnDrag}
          >
            <SortableContext
              items={columnOrder}
              strategy={horizontalListSortingStrategy}
            >
              <div
                id={headerId}
                ref={header}
                role="row"
                aria-rowindex={1}
                style={headerStyle}
              >
                {table.getHeaderGroups()[0].headers.map((item, index) => (
                  <GridHeader
                    key={item.id}
                    header={item}
                    index={index}
                    ids={orderedIds}
                    middleOffset={middleOffset}
                    sortCount={sorting.length}
                    suppressClick={suppressHeaderClick}
                    onAutoFit={fitColumn}
                  />
                ))}
              </div>
            </SortableContext>
          </DndContext>
        </div>
      )}
      <div
        ref={scroll}
        role="grid"
        aria-label={t("headers.name")}
        aria-rowcount={total}
        aria-owns={mobile ? undefined : `${headerId} ${bodyId}`}
        aria-colcount={mobile ? 1 : visibleColumns.length}
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
              columnsSignature={columnsSignature}
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
