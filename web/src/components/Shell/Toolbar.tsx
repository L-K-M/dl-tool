import {
  useEffect,
  useState,
  useSyncExternalStore,
  type MouseEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import {
  CheckCheck,
  ChevronDown,
  Columns3,
  Monitor,
  Moon,
  Pause,
  Pencil,
  Play,
  Plus,
  Search,
  Settings,
  Sun,
  Trash2,
  UserRound,
  X,
} from "lucide-react";
import { api } from "../../api/client";
import {
  applyTheme,
  readStoredTheme,
  storeTheme,
  type ThemeChoice,
} from "../../lib/theme";
import { useTasks, type Task } from "../../store/useTasks";
import { invalidateTaskList } from "../TaskGrid/TaskGrid";
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
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";

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

type QueueMove = "queue_top" | "queue_up" | "queue_down" | "queue_bottom";

/** Doc 09 §10.6: pause, resume and the queue moves are optimistic; the rest never are. */
const optimisticActions: ReadonlySet<BulkAction> = new Set([
  "pause",
  "resume",
  "queue_top",
  "queue_up",
  "queue_down",
  "queue_bottom",
]);
const maxIdsPerRequest = 500;
const nameFilterDelayMs = 250;
const maxNamedTasks = 5;
const wideToolbarQuery = "min-[1100px]:inline";
const themeCycle: readonly ThemeChoice[] = ["system", "light", "dark"];

/**
 * The toolbar's client-side name filter (doc 09 §2.5 item 7). The input text commits to
 * `value` after the debounce, so the grid re-filters at typing speed, not per keystroke.
 * Module scope, like the task store: the Toolbar owns the input, the grid owns the rows,
 * and both read one snapshot.
 */
const nameFilterListeners = new Set<() => void>();
let nameFilterText = "";
let nameFilterValue = "";
let nameFilterTimer: ReturnType<typeof setTimeout> | undefined;

function emitNameFilter(): void {
  for (const listener of nameFilterListeners) listener();
}

function setNameFilterText(next: string): void {
  nameFilterText = next;
  if (nameFilterTimer) clearTimeout(nameFilterTimer);
  nameFilterTimer = setTimeout(() => {
    nameFilterTimer = undefined;
    if (nameFilterValue !== nameFilterText) {
      nameFilterValue = nameFilterText;
      emitNameFilter();
    }
  }, nameFilterDelayMs);
}

/** Test and boot reset: clears both the text and any pending debounce. */
export function resetNameFilter(): void {
  if (nameFilterTimer) clearTimeout(nameFilterTimer);
  nameFilterTimer = undefined;
  nameFilterText = "";
  nameFilterValue = "";
  emitNameFilter();
}

export function useNameFilter(): { value: string; set: (v: string) => void } {
  const value = useSyncExternalStore(
    (onStoreChange) => {
      nameFilterListeners.add(onStoreChange);
      return () => nameFilterListeners.delete(onStoreChange);
    },
    () => nameFilterValue,
    () => nameFilterValue,
  );
  return { value, set: setNameFilterText };
}

/** Preceding task objects, restored verbatim when the server rejects the action. */
type Rollback = Task[];

function patchTasks(patches: Task[]): void {
  // hydrate merges patches into the SSE-fed store without touching rid or stats.
  useTasks.getState().hydrate(patches);
}

function optimisticPauseOrResume(
  action: "pause" | "resume",
  ids: string[],
): Rollback {
  const state = useTasks.getState();
  const patchable =
    action === "pause"
      ? new Set(["downloading", "seeding", "queued"])
      : new Set(["paused", "error"]);
  const rollback: Rollback = [];
  const patches: Task[] = [];
  for (const id of ids) {
    const task = state.tasks.get(id);
    if (!task || !patchable.has(task.state)) continue;
    rollback.push(task);
    patches.push(
      action === "pause"
        ? {
            ...task,
            state: "paused",
            download_rate: 0,
            upload_rate: 0,
            eta_seconds: null,
          }
        : { ...task, state: "downloading" },
    );
  }
  patchTasks(patches);
  return rollback;
}

/**
 * Renumber the queue as if the selected block had moved, keeping every other task's
 * relative order. The server and the next sync remain authoritative.
 */
function optimisticQueueMove(action: QueueMove, ids: string[]): Rollback {
  const tasks = useTasks.getState().tasks;
  const moving = new Set(ids);
  const positioned = [...tasks.values()]
    .filter((task) => task.queue_position !== null)
    .sort((a, b) => (a.queue_position ?? 0) - (b.queue_position ?? 0));
  const kept = positioned.filter((task) => !moving.has(task.id));
  const selected = positioned.filter((task) => moving.has(task.id));
  const blockStart = positioned.findIndex((task) => moving.has(task.id));
  let insert = kept.length;
  if (action === "queue_top") insert = 0;
  else if (action === "queue_up")
    insert = Math.max(0, blockStart === -1 ? kept.length : blockStart - 1);
  else if (action === "queue_down")
    insert = Math.min(
      kept.length,
      blockStart === -1 ? kept.length : blockStart + 1,
    );
  const merged = [...kept.slice(0, insert), ...selected, ...kept.slice(insert)];
  patchTasks(
    merged.map((task, index) => ({ ...task, queue_position: index + 1 })),
  );
  return positioned;
}

/** POST /api/v1/tasks/actions with {ids, action}; optimistic per doc 09 §10.6. */
export function useBulkAction(): (
  action: BulkAction,
  ids: string[],
) => Promise<void> {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: async (variables: { action: BulkAction; ids: string[] }) => {
      const { data, error } = await api.POST("/tasks/actions", {
        body: { ids: variables.ids, action: variables.action },
      });
      if (error || !data) throw new Error(error?.detail ?? "request failed");
      const failed = (data.results ?? []).filter((result) => !result.ok);
      if (failed.length > 0)
        throw new Error(failed[0]?.detail ?? "action refused");
      return data;
    },
    onMutate: (variables) => {
      if (!optimisticActions.has(variables.action)) return undefined;
      if (variables.action === "pause" || variables.action === "resume")
        return {
          rollback: optimisticPauseOrResume(variables.action, variables.ids),
        };
      if (
        variables.action === "queue_top" ||
        variables.action === "queue_up" ||
        variables.action === "queue_down" ||
        variables.action === "queue_bottom"
      )
        return {
          rollback: optimisticQueueMove(variables.action, variables.ids),
        };
      return undefined;
    },
    onError: (error, variables, context) => {
      if (context?.rollback) useTasks.getState().hydrate(context.rollback);
      toast.error(
        t("toolbar.actionFailed", {
          action: t(`toolbar.${variables.action}`),
          detail: error.message,
        }),
      );
    },
    onSettled: () => {
      void invalidateTaskList(queryClient);
    },
  });
  return async (action, ids) => {
    await mutation.mutateAsync({ action, ids }).catch(() => undefined);
  };
}

function Separator() {
  return (
    <span
      role="separator"
      aria-orientation="vertical"
      className="mx-1 h-5 w-px shrink-0 bg-border"
    />
  );
}

/**
 * A disabled control still names its reason. `disabled:pointer-events-none` lets the
 * pointer reach the wrapper, so the title tooltip shows while the button stays inert.
 */
function ToolButton({
  label,
  reason,
  onClick,
  icon,
}: {
  label: string;
  reason?: string;
  onClick?: () => void;
  icon: ReactNode;
}) {
  const button = (
    <Button
      variant="ghost"
      size="sm"
      disabled={reason !== undefined}
      aria-disabled={reason !== undefined ? "true" : undefined}
      onClick={onClick}
    >
      {icon}
      <span className={`hidden ${wideToolbarQuery}`}>{label}</span>
    </Button>
  );
  if (reason === undefined) return button;
  return (
    <span title={reason} className="inline-flex">
      {button}
    </span>
  );
}

function MenuButton({
  label,
  onSelect,
}: {
  label: string;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-sm hover:bg-muted"
      onClick={onSelect}
    >
      {label}
    </button>
  );
}

export interface ToolbarProps {
  /** Opens the shared remove-confirmation flow (also driven by Delete shortcuts). */
  onRemove: () => void;
  /** Ctrl/Cmd+F from the grid focuses this input. */
  filterInputRef: RefObject<HTMLInputElement | null>;
  user: { username: string } | null;
  onSignOut: () => void;
}

export function Toolbar({
  onRemove,
  filterInputRef,
  user,
  onSignOut,
}: ToolbarProps) {
  const { t } = useTranslation();
  const bulkAction = useBulkAction();
  const selectionCount = useTasks((state) => state.selection.size);
  const connection = useTasks((state) => state.connection);
  const completedCount = useTasks(
    (state) =>
      [...state.tasks.values()].filter((task) => task.state === "completed")
        .length,
  );
  const [filterText, setFilterText] = useState("");
  const [theme, setTheme] = useState<ThemeChoice>(readStoredTheme);
  const [removeOpen, setRemoveOpen] = useState(false);
  const [moveOpen, setMoveOpen] = useState(false);
  const [userOpen, setUserOpen] = useState(false);
  const streamDown = connection === "connecting" || connection === "offline";
  const actionReason = streamDown
    ? t("toolbar.reconnecting")
    : selectionCount === 0
      ? t("toolbar.selectFirst")
      : undefined;

  const run = (action: BulkAction) => {
    void bulkAction(action, [...useTasks.getState().selection]);
  };

  const clearCompleted = () => {
    const completed = [...useTasks.getState().tasks.values()]
      .filter((task) => task.state === "completed")
      .map((task) => task.id);
    const runChunks = async () => {
      for (let start = 0; start < completed.length; start += maxIdsPerRequest) {
        await bulkAction(
          "remove",
          completed.slice(start, start + maxIdsPerRequest),
        );
      }
    };
    void runChunks();
  };

  const cycleTheme = () => {
    const next =
      themeCycle[(themeCycle.indexOf(theme) + 1) % themeCycle.length] ??
      "system";
    applyTheme(next);
    storeTheme(next);
    setTheme(next);
  };

  const setFilter = (value: string) => {
    setFilterText(value);
    setNameFilterText(value);
  };

  const clearFilter = (event: MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    setFilter("");
    filterInputRef.current?.focus();
  };

  const ThemeIcon = theme === "dark" ? Moon : theme === "light" ? Sun : Monitor;

  return (
    <div className="flex h-full items-center gap-1 px-2">
      <span title={t("toolbar.comingAddDialog")} className="inline-flex">
        <Button size="sm" disabled aria-disabled="true">
          <Plus data-icon="inline-start" />
          <span className={`hidden ${wideToolbarQuery}`}>
            {t("toolbar.add")}
          </span>
        </Button>
      </span>
      <Separator />
      <ToolButton
        label={t("toolbar.start")}
        reason={actionReason}
        icon={<Play />}
        onClick={() => run("resume")}
      />
      <ToolButton
        label={t("toolbar.pause")}
        reason={actionReason}
        icon={<Pause />}
        onClick={() => run("pause")}
      />
      {/* A disabled trigger must not open its menu; the wrapper keeps the tooltip. */}
      <Popover
        open={actionReason !== undefined ? false : removeOpen}
        onOpenChange={setRemoveOpen}
      >
        <PopoverTrigger asChild>
          <span title={actionReason} className="inline-flex">
            <Button
              variant="ghost"
              size="sm"
              disabled={actionReason !== undefined}
              aria-disabled={actionReason !== undefined ? "true" : undefined}
            >
              <Trash2 />
              <span className={`hidden ${wideToolbarQuery}`}>
                {t("toolbar.remove")}
              </span>
              <ChevronDown data-icon="inline-end" />
            </Button>
          </span>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-56 p-1">
          {/* Doc 09 §10.6: the delete-files box is never pre-checked except after
              Shift+Delete, so both menu items open the same unticked dialog. */}
          <MenuButton label={t("toolbar.removeTasks")} onSelect={onRemove} />
          <MenuButton
            label={t("toolbar.removeTasksAndFiles")}
            onSelect={onRemove}
          />
        </PopoverContent>
      </Popover>
      <span title={t("toolbar.comingEditor")} className="inline-flex">
        <Button variant="ghost" size="sm" disabled aria-disabled="true">
          <Pencil />
          <span className={`hidden ${wideToolbarQuery}`}>
            {t("toolbar.edit")}
          </span>
        </Button>
      </span>
      <Separator />
      <Popover
        open={actionReason !== undefined ? false : moveOpen}
        onOpenChange={setMoveOpen}
      >
        <PopoverTrigger asChild>
          <span title={actionReason} className="inline-flex">
            <Button
              variant="ghost"
              size="sm"
              disabled={actionReason !== undefined}
              aria-disabled={actionReason !== undefined ? "true" : undefined}
            >
              <span className={`hidden ${wideToolbarQuery}`}>
                {t("toolbar.move")}
              </span>
              <ChevronDown data-icon="inline-end" />
            </Button>
          </span>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-40 p-1">
          {(
            [
              ["queue_top", "moveTop"],
              ["queue_up", "moveUp"],
              ["queue_down", "moveDown"],
              ["queue_bottom", "moveBottom"],
            ] as const
          ).map(([action, key]) => (
            <MenuButton
              key={action}
              label={t(`toolbar.${key}`)}
              onSelect={() => run(action)}
            />
          ))}
        </PopoverContent>
      </Popover>
      <ToolButton
        label={t("toolbar.clearCompleted")}
        reason={completedCount === 0 ? t("toolbar.noCompleted") : undefined}
        icon={<CheckCheck />}
        onClick={clearCompleted}
      />
      <Separator />
      <div className="relative ml-auto flex items-center">
        <Search
          size={14}
          aria-hidden="true"
          className="pointer-events-none absolute left-2 text-muted-foreground"
        />
        <Input
          ref={filterInputRef}
          type="search"
          value={filterText}
          onChange={(event) => setFilter(event.target.value)}
          placeholder={t("toolbar.filterPlaceholder")}
          aria-label={t("toolbar.filterLabel")}
          className="h-7 w-44 pr-7 pl-7"
        />
        {filterText !== "" && (
          <button
            type="button"
            onClick={clearFilter}
            aria-label={t("toolbar.clearFilter")}
            className="absolute right-1.5 flex size-5 items-center justify-center rounded text-muted-foreground hover:bg-muted"
          >
            <X size={12} aria-hidden="true" />
          </button>
        )}
      </div>
      <span title={t("toolbar.comingColumns")} className="inline-flex">
        <Button variant="ghost" size="sm" disabled aria-disabled="true">
          <Columns3 />
          <span className={`hidden ${wideToolbarQuery}`}>
            {t("toolbar.columns")}
          </span>
        </Button>
      </span>
      <Button
        variant="ghost"
        size="sm"
        onClick={cycleTheme}
        aria-label={t(`theme.${theme}`)}
        title={t(`theme.${theme}`)}
      >
        <ThemeIcon />
      </Button>
      <Button variant="ghost" size="icon-sm" asChild>
        <Link to="/settings/general" aria-label={t("toolbar.settings")}>
          <Settings />
        </Link>
      </Button>
      <Popover open={userOpen} onOpenChange={setUserOpen}>
        <PopoverTrigger asChild>
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={t("toolbar.userMenu")}
          >
            <UserRound />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="end" className="w-44 p-1">
          <p className="truncate px-2 py-1.5 text-xs text-muted-foreground">
            {user?.username ?? ""}
          </p>
          <MenuButton label={t("toolbar.signOut")} onSelect={onSignOut} />
        </PopoverContent>
      </Popover>
    </div>
  );
}

export interface RemoveConfirmDialogProps {
  open: boolean;
  ids: string[];
  /** Only Shift+Delete pre-checks the box (doc 09 §10.6). */
  preDeleteData: boolean;
  busy: boolean;
  onCancel: () => void;
  onConfirm: (deleteData: boolean) => void;
  /** Element to refocus on close; the grid selection is never cleared by closing. */
  restoreFocus: RefObject<HTMLElement | null>;
}

export function RemoveConfirmDialog({
  open,
  ids,
  preDeleteData,
  busy,
  onCancel,
  onConfirm,
  restoreFocus,
}: RemoveConfirmDialogProps) {
  const { t } = useTranslation();
  const [deleteData, setDeleteData] = useState(preDeleteData);
  const tasks = useTasks((state) => state.tasks);
  // A later open must not inherit the previous dialog's tick.
  useEffect(() => {
    if (open) setDeleteData(preDeleteData);
  }, [open, preDeleteData]);
  const named = ids.map((id) => ({ id, name: tasks.get(id)?.name ?? id }));
  const shown = named.slice(0, maxNamedTasks);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onCancel();
      }}
    >
      <DialogContent
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          restoreFocus.current?.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("removeDialog.title")}</DialogTitle>
          <DialogDescription>
            {t("removeDialog.affected", { count: ids.length })}
          </DialogDescription>
        </DialogHeader>
        <ul className="max-h-40 overflow-y-auto text-sm">
          {shown.map((entry) => (
            <li key={entry.id} className="truncate">
              {entry.name}
            </li>
          ))}
          {named.length > maxNamedTasks && (
            <li className="text-muted-foreground">
              {t("removeDialog.andMore", {
                count: named.length - maxNamedTasks,
              })}
            </li>
          )}
        </ul>
        {preDeleteData && (
          <p className="text-xs text-muted-foreground">
            {t("removeDialog.preTickedNote")}
          </p>
        )}
        <label className="flex items-center gap-2 text-sm">
          <Checkbox
            checked={deleteData}
            onCheckedChange={(value) => setDeleteData(value === true)}
          />
          {t("removeDialog.deleteFiles")}
        </label>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel}>
            {t("removeDialog.cancel")}
          </Button>
          <Button
            variant="destructive"
            disabled={busy}
            onClick={() => onConfirm(deleteData)}
          >
            {t("removeDialog.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** The §3.6 cheat sheet, opened with `?` from the grid. */
const SHORTCUTS: ReadonlyArray<{ keys: string; key: string }> = [
  { keys: "↑ ↓", key: "moveRow" },
  { keys: "Home End", key: "firstLastRow" },
  { keys: "PageUp PageDown", key: "pageRows" },
  { keys: "Ctrl+Home Ctrl+End", key: "firstLastCell" },
  { keys: "Space", key: "toggleSelect" },
  { keys: "Shift+Space", key: "selectRow" },
  { keys: "Ctrl/Cmd+A", key: "selectAll" },
  { keys: "Enter F2", key: "detail" },
  { keys: "Delete", key: "remove" },
  { keys: "Shift+Delete", key: "removeData" },
  { keys: "Ctrl/Cmd+F", key: "filter" },
  { keys: "Esc", key: "escape" },
  { keys: "?", key: "cheat" },
];

export function ShortcutSheet({
  open,
  onClose,
  restoreFocus,
}: {
  open: boolean;
  onClose: () => void;
  restoreFocus: RefObject<HTMLElement | null>;
}) {
  const { t } = useTranslation();
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
    >
      <DialogContent
        onCloseAutoFocus={(event) => {
          event.preventDefault();
          restoreFocus.current?.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("shortcuts.title")}</DialogTitle>
        </DialogHeader>
        <table className="text-sm">
          <tbody>
            {SHORTCUTS.map((shortcut) => (
              <tr key={shortcut.key}>
                <td className="py-0.5 pr-4 font-mono whitespace-nowrap">
                  {shortcut.keys}
                </td>
                <td className="py-0.5">{t(`shortcuts.${shortcut.key}`)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            {t("shortcuts.close")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
