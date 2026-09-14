import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type JSX,
} from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { DropdownMenu } from "radix-ui";
import { toast } from "sonner";
import { create } from "zustand";
import {
  ArrowUpDown,
  Check,
  ChevronDown,
  Columns3,
  Moon,
  Pause,
  Pencil,
  Play,
  Plus,
  Settings,
  Sun,
  Trash2,
  User,
  X,
} from "lucide-react";
import { api } from "../../api/client";
import {
  applyTheme,
  readStoredTheme,
  resolveTheme,
  storeTheme,
  type ThemeChoice,
} from "../../lib/theme";
import { selectFilterCounts, useTasks, type Task } from "../../store/useTasks";
import { useUiPrefs } from "../../store/useUiPrefs";
import { ColumnsMenu, useGridTable } from "../TaskGrid/ColumnsMenu";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";

export type BulkAction =
  | "pause"
  | "resume"
  | "remove"
  | "recheck"
  | "force_complete"
  | "queue_top"
  | "queue_up"
  | "queue_down"
  | "queue_bottom";

// Kept in step with TaskGrid's listKey; importing it would make the two
// component modules depend on each other in both directions.
const listKey = ["tasks"] as const;
const nameFilterDebounceMs = 250;
const optimisticActions = new Set<BulkAction>([
  "pause",
  "resume",
  "queue_top",
  "queue_up",
  "queue_down",
  "queue_bottom",
]);

interface ShellUiState {
  nameFilter: string;
  debouncedFilter: string;
  pending: ReadonlySet<string>;
  filterInput: HTMLInputElement | null;
}

export const useShellUi = create<ShellUiState>(() => ({
  nameFilter: "",
  debouncedFilter: "",
  pending: new Set(),
  filterInput: null,
}));

function setPending(ids: string[], on: boolean) {
  useShellUi.setState((state) => {
    const pending = new Set(state.pending);
    for (const id of ids) {
      if (on) pending.add(id);
      else pending.delete(id);
    }
    return { pending };
  });
}

/** Whether a bulk mutation is in flight for the task (the 1 px row shimmer). */
export function useTaskPending(id: string): boolean {
  return useShellUi((state) => state.pending.has(id));
}

/** The debounced name filter the grid applies to its name column. */
export function useDebouncedNameFilter(): string {
  return useShellUi((state) => state.debouncedFilter);
}

/** The client-side name filter of doc 09 §2.5 item 7; 250 ms debounce. */
export function useNameFilter(): { value: string; set: (v: string) => void } {
  const value = useShellUi((state) => state.nameFilter);
  useEffect(() => {
    const timer = setTimeout(
      () => useShellUi.setState({ debouncedFilter: value }),
      nameFilterDebounceMs,
    );
    return () => clearTimeout(timer);
  }, [value]);
  return {
    value,
    set: useCallback((v: string) => useShellUi.setState({ nameFilter: v }), []),
  };
}

/** Ctrl/Cmd+F's target: the filter input the toolbar registers. */
export function focusNameFilter(): void {
  useShellUi.getState().filterInput?.focus();
}

// The optimistic queue-move patch mirrors the store's reorderQueueMembers
// (internal/store/tasks.go): selected members keep their relative order and
// so do the unselected ones; a move rewrites only the boundary.
function optimisticQueueOrder(
  tasks: ReadonlyMap<string, Task>,
  ids: string[],
  action: BulkAction,
): Map<string, Partial<Task>> {
  const order = [...tasks.values()]
    .filter((task) => task.queue_position !== null)
    .sort(
      (a, b) =>
        a.queue_position! - b.queue_position! || a.id.localeCompare(b.id),
    )
    .map((task) => task.id);
  const selected = new Set(ids);
  let next = [...order];
  switch (action) {
    case "queue_top":
      next = [
        ...next.filter((id) => selected.has(id)),
        ...next.filter((id) => !selected.has(id)),
      ];
      break;
    case "queue_bottom":
      next = [
        ...next.filter((id) => !selected.has(id)),
        ...next.filter((id) => selected.has(id)),
      ];
      break;
    case "queue_up":
      for (let i = 1; i < next.length; i++)
        if (selected.has(next[i]) && !selected.has(next[i - 1]))
          [next[i - 1], next[i]] = [next[i], next[i - 1]];
      break;
    case "queue_down":
      for (let i = next.length - 2; i >= 0; i--)
        if (selected.has(next[i]) && !selected.has(next[i + 1]))
          [next[i], next[i + 1]] = [next[i + 1], next[i]];
      break;
  }
  return new Map(
    next.map((id, position) => [id, { queue_position: position + 1 }]),
  );
}

function optimisticPatch(
  action: BulkAction,
  ids: string[],
): Map<string, Partial<Task>> {
  switch (action) {
    case "pause":
      return new Map(ids.map((id) => [id, { state: "paused" }]));
    case "resume":
      return new Map(ids.map((id) => [id, { state: "queued" }]));
    default:
      return optimisticQueueOrder(useTasks.getState().tasks, ids, action);
  }
}

/** POST /api/v1/tasks/actions with {ids, action}. Optimistic for pause, resume and the queue moves. */
export function useBulkAction(): (
  action: BulkAction,
  ids: string[],
) => Promise<void> {
  const queryClient = useQueryClient();
  const { t } = useTranslation();
  const mutation = useMutation({
    mutationFn: async ({
      action,
      ids,
    }: {
      action: BulkAction;
      ids: string[];
    }) => {
      const { data, error } = await api.POST("/tasks/actions", {
        body: { ids, action },
      });
      if (error)
        throw new Error(
          error.detail ??
            error.title ??
            t("shell.actionFailed", { detail: error.type }),
        );
      return data.results;
    },
    onMutate: ({ action, ids }) => {
      setPending(ids, true);
      if (!optimisticActions.has(action)) return { previous: null };
      const tasks = useTasks.getState().tasks;
      const patches = optimisticPatch(action, ids);
      // A queue move rewrites positions of unselected members too, so the
      // rollback snapshot covers every id the patch touches.
      const previous = new Map<string, Task>();
      for (const id of patches.keys()) {
        const task = tasks.get(id);
        if (task) previous.set(id, task);
      }
      const merged = new Map(tasks);
      for (const [id, patch] of patches) {
        const base = merged.get(id);
        if (base) merged.set(id, { ...base, ...patch });
      }
      useTasks.setState({ tasks: merged });
      return { previous };
    },
    onSuccess: (results) => {
      for (const result of results ?? [])
        if (!result.ok)
          toast.error(
            result.detail ?? result.type ?? t("shell.actionFailedGeneric"),
          );
    },
    onError: (error, _variables, context) => {
      if (context?.previous) {
        const tasks = new Map(useTasks.getState().tasks);
        for (const [id, task] of context.previous) tasks.set(id, task);
        useTasks.setState({ tasks });
      }
      toast.error(error.message);
    },
    onSettled: (_data, _error, variables) => {
      setPending(variables.ids, false);
      void queryClient.invalidateQueries({ queryKey: listKey });
    },
  });
  // Failures surface through onError's toast; callers `void` the promise, so
  // it must never reject unhandled.
  return (action, ids) =>
    mutation
      .mutateAsync({ action, ids })
      .then(() => undefined)
      .catch(() => undefined);
}

/** The shell-level actions the grid's keydown listener dispatches to. */
export interface ShellActions {
  requestRemove: (ids: string[], deleteFiles: boolean) => void;
  focusFilter: () => void;
  showShortcuts: () => void;
  signOut: () => void;
  userName: string;
}

export const ShellActionsContext = createContext<ShellActions | null>(null);

export function useShellActions(): ShellActions {
  const actions = useContext(ShellActionsContext);
  if (!actions) throw new Error("useShellActions outside its provider");
  return actions;
}

export interface RemoveRequest {
  ids: string[];
  /** Pre-ticks the delete-files box; only the Shift+Delete flow passes true. */
  deleteFiles: boolean;
}

// Radix Dialog focuses its trigger on close, but shell dialogs open from
// keyboard shortcuts with no trigger, so restore focus to the element that
// opened them (usually a grid row). The element is captured during render
// because the focus scope's mount effect runs before this component's own
// effects and would already have moved focus into the dialog.
function useFocusRestore(open: boolean) {
  const elementRef = useRef<Element | null>(null);
  const wasOpenRef = useRef(false);
  if (open && !wasOpenRef.current) {
    elementRef.current = document.activeElement;
  }
  wasOpenRef.current = open;
  return (event: Event) => {
    event.preventDefault();
    if (elementRef.current instanceof HTMLElement) elementRef.current.focus();
  };
}

export function RemoveTasksDialog({
  request,
  onClose,
}: {
  request: RemoveRequest | null;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [deleteFiles, setDeleteFiles] = useState(false);
  const [busy, setBusy] = useState(false);
  const onCloseAutoFocus = useFocusRestore(request !== null);
  useEffect(() => {
    if (request) setDeleteFiles(request.deleteFiles);
  }, [request]);
  const tasks = useTasks((state) => state.tasks);
  const names = (request?.ids ?? []).map((id) => tasks.get(id)?.name ?? id);
  const confirm = async () => {
    if (!request || busy) return;
    setBusy(true);
    const removed: string[] = [];
    const failures: string[] = [];
    try {
      // Every id gets its own DELETE: a transport failure on one must not
      // skip the rest, and each failure reports its own detail.
      const settled = await Promise.allSettled(
        request.ids.map(async (id) => {
          const { error } = await api.DELETE("/tasks/{id}", {
            params: { path: { id }, query: { delete_data: deleteFiles } },
          });
          if (error)
            throw Object.assign(
              new Error(String(error.detail ?? error.title ?? id)),
              { fromApi: true },
            );
          return id;
        }),
      );
      for (const result of settled) {
        if (result.status === "fulfilled") removed.push(result.value);
        else
          failures.push(
            result.reason instanceof Error &&
              (result.reason as { fromApi?: boolean }).fromApi
              ? result.reason.message
              : t("shell.networkError"),
          );
      }
    } finally {
      setBusy(false);
    }
    if (removed.length) {
      // Route through applySync so the store drops the rows and their
      // selection entries exactly the way an SSE tasks_removed would.
      const state = useTasks.getState();
      state.applySync({
        rid: state.rid,
        full_update: false,
        seq_gap: false,
        tasks: {},
        tasks_removed: removed,
        stats: state.stats,
      });
    }
    for (const failure of failures)
      toast.error(t("shell.removeDialog.failed", { detail: failure }));
    onClose();
  };
  return (
    <Dialog
      open={request !== null}
      onOpenChange={(open) => {
        // Escape and outside clicks are ignored mid-flight: the buttons are
        // disabled, so the dialog must not pretend to be idle either.
        if (!open && !busy) onClose();
      }}
    >
      <DialogContent onCloseAutoFocus={onCloseAutoFocus}>
        <DialogHeader>
          <DialogTitle>
            {t("shell.removeDialog.title", { count: request?.ids.length ?? 0 })}
          </DialogTitle>
          <DialogDescription asChild>
            <div>
              {request?.deleteFiles ? (
                <p>{t("shell.removeDialog.preChecked")}</p>
              ) : null}
              <ul className="max-h-40 list-disc overflow-y-auto pl-5">
                {names.map((name, index) => (
                  <li key={request?.ids[index] ?? index}>{name}</li>
                ))}
              </ul>
            </div>
          </DialogDescription>
        </DialogHeader>
        <label className="flex items-center gap-2 text-sm">
          <Checkbox
            checked={deleteFiles}
            onCheckedChange={(checked) => setDeleteFiles(checked === true)}
          />
          {t("shell.removeDialog.deleteFiles")}
        </label>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            {t("shell.removeDialog.cancel")}
          </Button>
          <Button
            variant="destructive"
            onClick={() => void confirm()}
            disabled={busy}
          >
            {t("shell.removeDialog.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

const SHORTCUT_ROWS: [string, string][] = [
  ["↑ ↓", "shell.shortcuts.navigate"],
  ["Home End", "shell.shortcuts.firstLast"],
  ["PageUp PageDown", "shell.shortcuts.page"],
  ["Ctrl+Home Ctrl+End", "shell.shortcuts.firstLastCell"],
  ["Space", "shell.shortcuts.toggleRow"],
  ["Shift+Space", "shell.shortcuts.selectRow"],
  ["Ctrl/Cmd+A", "shell.shortcuts.selectAll"],
  ["Enter F2", "shell.shortcuts.detail"],
  ["Delete", "shell.shortcuts.remove"],
  ["Shift+Delete", "shell.shortcuts.removeData"],
  ["Ctrl/Cmd+F", "shell.shortcuts.filter"],
  ["Esc", "shell.shortcuts.escape"],
  ["?", "shell.shortcuts.help"],
];

export function ShortcutsDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const onCloseAutoFocus = useFocusRestore(open);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent onCloseAutoFocus={onCloseAutoFocus}>
        <DialogHeader>
          <DialogTitle>{t("shell.shortcuts.title")}</DialogTitle>
        </DialogHeader>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
          {SHORTCUT_ROWS.map(([keys, label]) => (
            <div key={label} className="contents">
              <dt className="font-mono text-muted-foreground">{keys}</dt>
              <dd>{t(label)}</dd>
            </div>
          ))}
        </dl>
      </DialogContent>
    </Dialog>
  );
}

const menuContentClass =
  "z-50 min-w-36 rounded-lg bg-popover p-1 text-sm text-popover-foreground shadow-md ring-1 ring-foreground/10";
const menuItemClass =
  "flex cursor-default items-center gap-1.5 rounded-md px-1.5 py-1 outline-none select-none focus:bg-accent focus:text-accent-foreground data-disabled:pointer-events-none data-disabled:opacity-50";
const separator = (
  <span aria-hidden="true" className="mx-1 w-px self-stretch bg-border" />
);
// Doc 09 section 2.5: icons plus text at >= 1100 px, icon-only below. The
// label span drops out of the accessibility tree, so every labeled control
// also carries an aria-label (doc 09 section 10: icon-only buttons name
// themselves).
const iconLabelClass = "max-[1100px]:hidden";

export function Toolbar(): JSX.Element {
  const { t } = useTranslation();
  const actions = useShellActions();
  const bulkAction = useBulkAction();
  const selection = useTasks((state) => state.selection);
  const connection = useTasks((state) => state.connection);
  const completedCount = useTasks(
    (state) => selectFilterCounts(state).completed,
  );
  const { value: filter, set: setFilter } = useNameFilter();
  const [theme, setTheme] = useState<ThemeChoice>(() => readStoredTheme());
  const registerFilter = useCallback(
    (node: HTMLInputElement | null) =>
      useShellUi.setState({ filterInput: node }),
    [],
  );
  const selectedIds = [...selection];
  const hasSelection = selectedIds.length > 0;
  const live = connection === "live" || connection === "polling";
  const disabledReason = !live
    ? t("shell.reconnecting")
    : !hasSelection
      ? t("shell.needsSelection")
      : undefined;
  const selectionDisabled = disabledReason !== undefined;
  const completedDisabled = !live;
  const selectionProps = {
    disabled: selectionDisabled,
    "aria-disabled": selectionDisabled || undefined,
    title: disabledReason,
  } as const;
  const gridTable = useGridTable((state) => state.table);
  const toggleTheme = () => {
    const next = resolveTheme(theme) === "dark" ? "light" : "dark";
    storeTheme(next);
    applyTheme(next);
    setTheme(next);
    // lib/theme owns the class; the prefs document only records the choice.
    useUiPrefs.getState().patch({ theme: next });
  };
  return (
    <div className="flex h-full min-w-0 items-center gap-1 overflow-x-auto border-b border-border px-2">
      <Button
        variant="default"
        size="sm"
        disabled
        aria-disabled="true"
        aria-label={t("shell.add")}
        title={t("shell.comingAdd")}
      >
        <Plus aria-hidden="true" />{" "}
        <span className={iconLabelClass}>{t("shell.add")}</span>
      </Button>
      {separator}
      <Button
        variant="ghost"
        size="sm"
        {...selectionProps}
        aria-label={t("shell.start")}
        onClick={() => void bulkAction("resume", selectedIds)}
      >
        <Play aria-hidden="true" />{" "}
        <span className={iconLabelClass}>{t("shell.start")}</span>
      </Button>
      <Button
        variant="ghost"
        size="sm"
        {...selectionProps}
        aria-label={t("shell.pause")}
        onClick={() => void bulkAction("pause", selectedIds)}
      >
        <Pause aria-hidden="true" />{" "}
        <span className={iconLabelClass}>{t("shell.pause")}</span>
      </Button>
      <DropdownMenu.Root>
        <DropdownMenu.Trigger asChild>
          <Button
            variant="ghost"
            size="sm"
            {...selectionProps}
            aria-label={t("shell.remove")}
          >
            <Trash2 aria-hidden="true" />{" "}
            <span className={iconLabelClass}>{t("shell.remove")}</span>{" "}
            <ChevronDown aria-hidden="true" />
          </Button>
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content className={menuContentClass} align="start">
            <DropdownMenu.Item
              className={menuItemClass}
              onSelect={() => actions.requestRemove(selectedIds, false)}
            >
              {t("shell.removeTask")}
            </DropdownMenu.Item>
            <DropdownMenu.Item
              className={menuItemClass}
              onSelect={() => actions.requestRemove(selectedIds, false)}
            >
              {t("shell.removeTaskAndFiles")}
            </DropdownMenu.Item>
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
      <Button
        variant="ghost"
        size="sm"
        disabled
        aria-disabled="true"
        aria-label={t("shell.edit")}
        title={t("shell.comingEdit")}
      >
        <Pencil aria-hidden="true" />{" "}
        <span className={iconLabelClass}>{t("shell.edit")}</span>
      </Button>
      {separator}
      <DropdownMenu.Root>
        <DropdownMenu.Trigger asChild>
          <Button
            variant="ghost"
            size="sm"
            {...selectionProps}
            aria-label={t("shell.move")}
          >
            <ArrowUpDown aria-hidden="true" />{" "}
            <span className={iconLabelClass}>{t("shell.move")}</span>{" "}
            <ChevronDown aria-hidden="true" />
          </Button>
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content className={menuContentClass} align="start">
            {(
              [
                ["queue_top", "shell.moveTop"],
                ["queue_up", "shell.moveUp"],
                ["queue_down", "shell.moveDown"],
                ["queue_bottom", "shell.moveBottom"],
              ] as const
            ).map(([action, label]) => (
              <DropdownMenu.Item
                key={action}
                className={menuItemClass}
                onSelect={() => void bulkAction(action, selectedIds)}
              >
                {t(label)}
              </DropdownMenu.Item>
            ))}
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
      <Button
        variant="ghost"
        size="sm"
        disabled={completedDisabled || completedCount === 0}
        aria-disabled={completedDisabled || completedCount === 0 || undefined}
        title={
          completedDisabled
            ? t("shell.reconnecting")
            : completedCount === 0
              ? t("shell.noCompleted")
              : undefined
        }
        aria-label={t("shell.clearCompleted")}
        onClick={() =>
          actions.requestRemove(
            [...useTasks.getState().tasks.values()]
              .filter((task) => task.state === "completed")
              .map((task) => task.id),
            false,
          )
        }
      >
        <Check aria-hidden="true" />{" "}
        <span className={iconLabelClass}>{t("shell.clearCompleted")}</span>
      </Button>
      {separator}
      <span className="relative ml-auto flex min-w-0 items-center">
        <Input
          ref={registerFilter}
          value={filter}
          aria-label={t("shell.filterLabel")}
          placeholder={t("shell.filterLabel")}
          onChange={(event) => setFilter(event.target.value)}
          className="h-7 w-44 min-w-16 max-[1100px]:w-28"
        />
        {filter ? (
          <button
            type="button"
            aria-label={t("shell.filterClear")}
            onClick={() => setFilter("")}
            className="absolute right-1 text-muted-foreground"
          >
            <X size={14} aria-hidden="true" />
          </button>
        ) : null}
      </span>
      {separator}
      {gridTable ? (
        <ColumnsMenu table={gridTable} />
      ) : (
        <Button
          variant="ghost"
          size="sm"
          disabled
          aria-disabled="true"
          aria-label={t("shell.columns")}
          title={t("shell.columns")}
        >
          <Columns3 aria-hidden="true" />{" "}
          <span className={iconLabelClass}>{t("shell.columns")}</span>{" "}
          <ChevronDown aria-hidden="true" />
        </Button>
      )}
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label={t("shell.themeToggle")}
        title={t("shell.themeToggle")}
        onClick={toggleTheme}
      >
        {resolveTheme(theme) === "dark" ? (
          <Sun aria-hidden="true" />
        ) : (
          <Moon aria-hidden="true" />
        )}
      </Button>
      <Button variant="ghost" size="icon-sm" asChild>
        <Link to="/settings/general" aria-label={t("shell.settings")}>
          <Settings aria-hidden="true" />
        </Link>
      </Button>
      <DropdownMenu.Root>
        <DropdownMenu.Trigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={t("shell.userMenu")}
            title={actions.userName || t("shell.userMenu")}
          >
            <User aria-hidden="true" />
          </Button>
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content className={menuContentClass} align="end">
            <DropdownMenu.Label className="px-1.5 py-1 text-xs text-muted-foreground">
              {actions.userName}
            </DropdownMenu.Label>
            <DropdownMenu.Item
              className={menuItemClass}
              onSelect={() => actions.signOut()}
            >
              {t("shell.signOut")}
            </DropdownMenu.Item>
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
    </div>
  );
}
