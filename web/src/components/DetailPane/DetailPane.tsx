import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type JSX,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Copy, FolderOpen, Pencil } from "lucide-react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
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
import { useTasks, type Task } from "../../store/useTasks";
import { useUiPrefs } from "../../store/useUiPrefs";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "../ui/tabs";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { FolderBrowserDialog } from "../FolderBrowser/FolderBrowserDialog";
import {
  buildTree,
  FileTree,
  type FileChange,
  type FilePriority,
} from "../FileTree/FileTree";
import { TaskProgress } from "../TaskGrid/TaskGrid";

initI18n();

export type DetailTab =
  "general" | "transfer" | "trackers" | "peers" | "files" | "log";

const TAB_ORDER: DetailTab[] = [
  "general",
  "transfer",
  "trackers",
  "peers",
  "files",
  "log",
];
const BITTORRENT_KINDS = new Set(["magnet", "torrent"]);
const missing = "—";
const MIN_HEIGHT = 160;
const SPARKLINE_SAMPLES = 60;
const EVENTS_PAGE = 200;
const MAX_EVENT_PAGES = 25;
const FILTER_DEBOUNCE_MS = 250;

/** The signed-in operator's username for the "Added by" field; App.tsx provides it. */
export const OperatorContext = createContext<string | null>(null);

/** Tabs that do not apply to the selected task are hidden, never disabled.
 *  trackers and peers require source_kind of 'magnet' or 'torrent'. */
export function visibleTabs(task: Task): DetailTab[] {
  if (BITTORRENT_KINDS.has(task.source_kind)) return [...TAB_ORDER];
  return TAB_ORDER.filter((tab) => tab !== "trackers" && tab !== "peers");
}

type Tracker = components["schemas"]["TrackerDTO"];
type Peer = components["schemas"]["PeerDTO"];
type TaskEvent = components["schemas"]["TaskEventDTO"];
type TaskFile = components["schemas"]["TaskFileDTO"];
type FilesBody = components["schemas"]["ListTaskFilesOutputBody"];

const filesKey = (id: string) => ["task-files", id] as const;
const trackersKey = (id: string) => ["task-trackers", id] as const;
const peersKey = (id: string) => ["task-peers", id] as const;
const eventsKey = (id: string) => ["task-events", id] as const;

function problemDetail(
  error: { detail?: string; title?: string; type?: string } | undefined,
): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

async function copyText(text: string, copied: string, failed: string) {
  try {
    await navigator.clipboard.writeText(text);
    toast.success(copied);
  } catch {
    toast.error(failed);
  }
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const { t } = useTranslation();
  return (
    <Button
      variant="ghost"
      size="icon-xs"
      aria-label={label}
      onClick={() =>
        void copyText(text, t("detail.copied"), t("detail.copyFailed"))
      }
    >
      <Copy aria-hidden />
    </Button>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="flex min-w-0 items-center gap-1">{children}</dd>
    </div>
  );
}

/** The five-dot health indicator: lit dots scale with connected seeders,
 *  and a task with no known peers lights none. */
function healthDots(task: Task): number {
  if (task.total_peers === 0) return 0;
  if (task.connected_seeders === 0) return 1;
  if (task.connected_seeders < 3) return 2;
  if (task.connected_seeders < 6) return 3;
  if (task.connected_seeders < 10) return 4;
  return 5;
}

function Health({ task }: { task: Task }) {
  const { t } = useTranslation();
  const dots = healthDots(task);
  return (
    <span
      role="img"
      title={t("detail.fields.healthTip", {
        seeders: task.connected_seeders,
        peers: task.total_peers,
      })}
      aria-label={t("detail.fields.health")}
      className="inline-flex gap-0.5"
    >
      {[0, 1, 2, 3, 4].map((dot) => (
        <span
          key={dot}
          aria-hidden
          className="inline-block size-2 rounded-full"
          style={{
            background: dot < dots ? "var(--ok)" : "var(--progress-track)",
          }}
        />
      ))}
    </span>
  );
}

function useTaskPatch(task: Task) {
  const { t } = useTranslation();
  return useMutation({
    mutationFn: async (body: { name?: string; destination?: string }) => {
      const { data, error } = await api.PATCH("/tasks/{id}", {
        params: { path: { id: task.id } },
        body,
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
      return data;
    },
    onSuccess: (data) => useTasks.getState().hydrate([data]),
    onError: (error) =>
      toast.error(t("shell.actionFailed", { detail: error.message })),
  });
}

function GeneralPanel({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const operator = useContext(OperatorContext);
  const patch = useTaskPatch(task);
  const nameInput = useRef<HTMLInputElement>(null);
  const [renaming, setRenaming] = useState(false);
  const [draft, setDraft] = useState(task.name);
  useEffect(() => {
    // A failed rename must not leave the rejected draft on screen.
    if (!renaming || patch.isError) setDraft(task.name);
  }, [renaming, task.name, patch.isError]);
  const [browse, setBrowse] = useState<"open" | "change" | null>(null);
  const rename = () => {
    const name = draft.trim();
    setRenaming(false);
    if (name && name !== task.name) patch.mutate({ name });
  };
  const remaining =
    task.total_bytes === null
      ? null
      : Math.max(0, task.total_bytes - task.completed_bytes);
  const when = (value: string | null) =>
    value === null ? (
      missing
    ) : (
      <span title={formatWhen(value, new Date(), locale)}>
        {formatAbsolute(value, locale)}
      </span>
    );
  return (
    <>
      <dl className="grid grid-cols-[minmax(120px,180px)_minmax(0,1fr)] gap-x-3 gap-y-1 text-sm">
        <Field label={t("detail.fields.name")}>
          <Input
            ref={nameInput}
            aria-label={t("detail.fields.name")}
            value={draft}
            readOnly={!renaming}
            title={task.name}
            onChange={(event) => setDraft(event.target.value)}
            onBlur={() => {
              if (renaming) rename();
            }}
            onKeyDown={(event) => {
              if (!renaming) return;
              if (event.key === "Enter") rename();
              if (event.key === "Escape") {
                setDraft(task.name);
                setRenaming(false);
              }
            }}
            className={
              renaming
                ? undefined
                : "border-transparent bg-transparent shadow-none"
            }
          />
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={t("detail.fields.rename")}
            onClick={() => {
              setRenaming(true);
              nameInput.current?.focus();
              nameInput.current?.select();
            }}
          >
            <Pencil aria-hidden />
          </Button>
        </Field>
        <Field label={t("detail.fields.destination")}>
          <span className="truncate" title={task.destination}>
            {task.destination || missing}
          </span>
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={t("detail.fields.openFolder")}
            onClick={() => setBrowse("open")}
          >
            <FolderOpen aria-hidden />
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setBrowse("change")}>
            {t("detail.fields.change")}
          </Button>
        </Field>
        {task.requested_destination !== null &&
        task.requested_destination !== task.destination ? (
          <Field label={t("detail.fields.requestedDestination")}>
            <span className="truncate" title={task.requested_destination}>
              {task.requested_destination}
            </span>
          </Field>
        ) : null}
        <Field label={t("detail.fields.size")}>
          {t("detail.fields.sizeValue", {
            total: formatBytes(task.total_bytes, locale),
            done: formatBytes(task.completed_bytes, locale),
            remaining: formatBytes(remaining, locale),
          })}
        </Field>
        <Field label={t("detail.fields.addedBy")}>{operator ?? missing}</Field>
        <Field label={t("detail.fields.sourceUri")}>
          {task.source_uri === null ? (
            missing
          ) : (
            <>
              <span className="truncate" title={task.source_uri}>
                {task.source_uri}
              </span>
              <CopyButton
                text={task.source_uri}
                label={t("detail.fields.copySource")}
              />
            </>
          )}
        </Field>
        <Field label={t("detail.fields.taskType")}>{task.source_kind}</Field>
        <Field label={t("detail.fields.engine")}>{task.engine}</Field>
        <Field label={t("detail.fields.infoHashV1")}>
          <span className="truncate font-mono" title={task.infohash_v1 ?? ""}>
            {task.infohash_v1 ?? missing}
          </span>
        </Field>
        <Field label={t("detail.fields.infoHashV2")}>
          <span className="truncate font-mono" title={task.infohash_v2 ?? ""}>
            {task.infohash_v2 ?? missing}
          </span>
        </Field>
        <Field label={t("detail.fields.category")}>
          {task.category ?? missing}
        </Field>
        <Field label={t("detail.fields.tags")}>
          {task.tags?.length ? task.tags.join(", ") : missing}
        </Field>
        <Field label={t("detail.fields.addedAt")}>{when(task.added_at)}</Field>
        <Field label={t("detail.fields.startedAt")}>
          {when(task.started_at)}
        </Field>
        <Field label={t("detail.fields.completedAt")}>
          {when(task.completed_at)}
        </Field>
        <Field label={t("detail.fields.health")}>
          <Health task={task} />
        </Field>
      </dl>
      <FolderBrowserDialog
        open={browse !== null}
        initialPath={task.destination || undefined}
        onOpenChange={(open) => {
          if (!open) setBrowse(null);
        }}
        onSelect={(path) => {
          if (browse === "change" && path !== task.destination)
            patch.mutate({ destination: path });
        }}
      />
    </>
  );
}

function RateSparkline({ task }: { task: Task }) {
  const { t } = useTranslation();
  const [samples, setSamples] = useState<{ down: number; up: number }[]>([]);
  const id = task.id;
  useEffect(() => setSamples([]), [id]);
  // A sample lands on every task delta; the store ticks about once a second,
  // so the window covers the last minute of both rates (doc 09 §6).
  useEffect(() => {
    setSamples((previous) =>
      [...previous, { down: task.download_rate, up: task.upload_rate }].slice(
        -SPARKLINE_SAMPLES,
      ),
    );
  }, [id, task.download_rate, task.upload_rate, task.updated_at]);
  const max = Math.max(1, ...samples.map((s) => Math.max(s.down, s.up)));
  const points = (pick: (s: { down: number; up: number }) => number) =>
    samples
      .map(
        (s, index) =>
          `${(index / Math.max(1, samples.length - 1)) * 100},${32 - (pick(s) / max) * 30}`,
      )
      .join(" ");
  return (
    <svg
      viewBox="0 0 100 32"
      preserveAspectRatio="none"
      role="img"
      aria-label={t("detail.transfer.sparkline")}
      data-testid="rate-sparkline"
      className="h-8 w-full"
    >
      <polyline
        points={points((s) => s.down)}
        fill="none"
        stroke="var(--accent)"
        strokeWidth="1.5"
      />
      <polyline
        points={points((s) => s.up)}
        fill="none"
        stroke="var(--ok)"
        strokeWidth="1.5"
      />
    </svg>
  );
}

function TransferPanel({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const limit = (value: number) =>
    value === 0 ? "∞" : formatRate(value, locale);
  return (
    <div className="flex flex-col gap-2 text-sm">
      <div>
        <TaskProgress task={task} />
      </div>
      {task.state === "extracting" ? (
        <div>
          <span
            role="progressbar"
            aria-label={t("detail.transfer.extraction")}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={(task.unzip_progress ?? 0) * 100}
            className="block h-2 overflow-hidden rounded bg-muted"
          >
            <span
              className="block h-full bg-primary"
              style={{ width: `${(task.unzip_progress ?? 0) * 100}%` }}
            />
          </span>
        </div>
      ) : null}
      <dl className="grid grid-cols-2 gap-x-6 gap-y-1 sm:grid-cols-3 [&_dt]:text-muted-foreground">
        <div>
          <dt>{t("detail.transfer.downloaded")}</dt>
          <dd>{formatBytes(task.completed_bytes, locale)}</dd>
        </div>
        <div>
          <dt>{t("detail.transfer.uploaded")}</dt>
          <dd>{formatBytes(task.uploaded_bytes, locale)}</dd>
        </div>
        <div>
          <dt>{t("detail.transfer.shareRatio")}</dt>
          <dd>{formatRatio(task.ratio, locale)}</dd>
        </div>
        <div>
          <dt>{t("detail.transfer.downSpeed")}</dt>
          <dd>{formatRate(task.download_rate, locale)}</dd>
        </div>
        <div>
          <dt>{t("detail.transfer.upSpeed")}</dt>
          <dd>{formatRate(task.upload_rate, locale)}</dd>
        </div>
        <div>
          <dt>{t("detail.transfer.limits")}</dt>
          <dd>
            {t("detail.transfer.limitsValue", {
              down: limit(task.dl_limit),
              up: limit(task.ul_limit),
            })}
          </dd>
        </div>
        <div>
          <dt>{t("detail.transfer.eta")}</dt>
          <dd>
            {task.eta_seconds === 0
              ? missing
              : formatEta(task.eta_seconds, locale)}
          </dd>
        </div>
        <div>
          <dt>{t("detail.transfer.connections")}</dt>
          <dd>
            {t("detail.transfer.connectionsValue", {
              seeders: task.connected_seeders,
              leechers: task.connected_leechers,
              peers: task.total_peers,
            })}
          </dd>
        </div>
        <div>
          <dt>{t("detail.transfer.queuePosition")}</dt>
          <dd>
            {task.queue_position === null
              ? missing
              : new Intl.NumberFormat(locale).format(task.queue_position)}
          </dd>
        </div>
        <div>
          <dt>{t("detail.transfer.sequential")}</dt>
          <dd>{task.sequential ? t("detail.yes") : t("detail.no")}</dd>
        </div>
      </dl>
      <RateSparkline task={task} />
    </div>
  );
}

/** The engine's synthetic DHT, PeX and LSD rows cannot be removed (doc 05 §5.9). */
const isPseudoTracker = (tracker: Tracker) => tracker.url.startsWith("**");

function TrackersPanel({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [checked, setChecked] = useState<ReadonlySet<string>>(new Set());
  const [adding, setAdding] = useState(false);
  const [urls, setUrls] = useState("");
  const query = useQuery({
    // The task's own delta is the refetch trigger — no timer (task T048).
    // keepPreviousData keeps the table mounted while the new tick loads.
    queryKey: [...trackersKey(task.id), task.updated_at],
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => {
      const { data, error } = await api.GET("/tasks/{id}/trackers", {
        params: { path: { id: task.id } },
        signal,
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
      return data.trackers ?? [];
    },
  });
  const trackers = query.data ?? [];
  const fail = (detail: string | undefined) =>
    toast.error(
      t("shell.actionFailed", { detail: detail ?? t("shell.networkError") }),
    );
  const addMutation = useMutation({
    mutationFn: async (list: string[]) => {
      const { error } = await api.POST("/tasks/{id}/trackers", {
        params: { path: { id: task.id } },
        body: { urls: list },
      });
      if (error)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
    },
    onSuccess: async () => {
      setAdding(false);
      setUrls("");
      await queryClient.invalidateQueries({
        queryKey: trackersKey(task.id),
      });
    },
    onError: (error) => fail(error.message),
  });
  const removeMutation = useMutation({
    mutationFn: async (list: string[]) => {
      const { error } = await api.DELETE("/tasks/{id}/trackers", {
        params: { path: { id: task.id }, query: { url: list } },
      });
      if (error)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
    },
    onSuccess: async () => {
      setChecked(new Set());
      await queryClient.invalidateQueries({
        queryKey: trackersKey(task.id),
      });
    },
    onError: (error) => fail(error.message),
  });
  const addTrackers = () => {
    const list = urls
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line !== "");
    if (list.length === 0) return;
    addMutation.mutate(list);
  };
  const removeChecked = () => {
    const list = [...checked].filter(
      (url) => !trackers.some((tr) => tr.url === url && isPseudoTracker(tr)),
    );
    if (list.length === 0) return;
    removeMutation.mutate(list);
  };
  const toggle = (url: string) =>
    setChecked((previous) => {
      const next = new Set(previous);
      if (next.has(url)) next.delete(url);
      else next.add(url);
      return next;
    });
  return (
    <div className="flex flex-col gap-1 text-sm">
      <div className="flex items-center gap-1">
        <Button variant="ghost" size="sm" onClick={() => setAdding(!adding)}>
          {t("detail.trackers.add")}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          disabled={checked.size === 0 || removeMutation.isPending}
          onClick={() => removeChecked()}
        >
          {t("detail.trackers.remove")}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          disabled={checked.size === 0}
          onClick={() =>
            void copyText(
              [...checked].join("\n"),
              t("detail.copied"),
              t("detail.copyFailed"),
            )
          }
        >
          {t("detail.trackers.copyUrl")}
        </Button>
      </div>
      {adding ? (
        <div className="flex flex-col gap-1">
          <textarea
            aria-label={t("detail.trackers.urlsLabel")}
            value={urls}
            onChange={(event) => setUrls(event.target.value)}
            rows={3}
            className="w-full rounded-lg border border-input bg-transparent px-2.5 py-1 text-sm"
          />
          <div className="flex gap-1">
            <Button
              size="sm"
              disabled={addMutation.isPending}
              onClick={() => addTrackers()}
            >
              {t("detail.trackers.addConfirm")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setAdding(false)}
            >
              {t("detail.trackers.cancel")}
            </Button>
          </div>
        </div>
      ) : null}
      {query.isError ? (
        <p role="alert">{t("detail.loadError")}</p>
      ) : (
        <table className="w-full text-left">
          <thead>
            <tr className="text-muted-foreground">
              <th className="w-6" />
              <th>{t("detail.trackers.url")}</th>
              <th>{t("detail.trackers.status")}</th>
              <th className="text-right">{t("detail.trackers.seeds")}</th>
              <th className="text-right">{t("detail.trackers.peers")}</th>
              <th>{t("detail.trackers.message")}</th>
              <th className="text-right">
                {t("detail.trackers.nextAnnounce")}
              </th>
            </tr>
          </thead>
          <tbody>
            {trackers.map((tracker) => {
              const pseudo = isPseudoTracker(tracker);
              return (
                <tr key={tracker.url}>
                  <td>
                    {pseudo ? null : (
                      <input
                        type="checkbox"
                        aria-label={t("detail.trackers.select", {
                          url: tracker.url,
                        })}
                        checked={checked.has(tracker.url)}
                        onChange={() => toggle(tracker.url)}
                      />
                    )}
                  </td>
                  <td className="max-w-0 truncate" title={tracker.url}>
                    {tracker.url}
                  </td>
                  <td>{tracker.status}</td>
                  <td className="text-right tabular-nums">
                    {tracker.seeds ?? missing}
                  </td>
                  <td className="text-right tabular-nums">
                    {tracker.peers ?? missing}
                  </td>
                  <td className="max-w-0 truncate" title={tracker.message}>
                    {tracker.message || missing}
                  </td>
                  <td className="text-right tabular-nums">
                    {tracker.update_timer_seconds === null
                      ? missing
                      : formatEta(tracker.update_timer_seconds, i18n.language)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

/** qBittorrent's letter flags, passed through verbatim by the peers endpoint
 *  (internal/engine/qbittorrent/peers.go). Order follows the engine's legend. */
const PEER_FLAG_LEGEND: [string, string][] = [
  ["D", "detail.peers.flagD"],
  ["d", "detail.peers.flagd"],
  ["U", "detail.peers.flagU"],
  ["u", "detail.peers.flagu"],
  ["O", "detail.peers.flagO"],
  ["S", "detail.peers.flagS"],
  ["I", "detail.peers.flagI"],
  ["K", "detail.peers.flagK"],
  ["?", "detail.peers.flagQuestion"],
  ["X", "detail.peers.flagX"],
  ["H", "detail.peers.flagH"],
  ["E", "detail.peers.flagE"],
  ["e", "detail.peers.flage"],
  ["P", "detail.peers.flagP"],
  ["L", "detail.peers.flagL"],
];

function PeersPanel({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const [picked, setPicked] = useState<string | null>(null);
  const query = useQuery({
    queryKey: [...peersKey(task.id), task.updated_at],
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => {
      const { data, error } = await api.GET("/tasks/{id}/peers", {
        params: { path: { id: task.id } },
        signal,
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
      return data.peers ?? [];
    },
  });
  const peers = query.data ?? [];
  return (
    <div className="flex flex-col gap-1 text-sm">
      <div className="flex items-center gap-1">
        <Button
          variant="ghost"
          size="sm"
          disabled={picked === null}
          onClick={() =>
            picked !== null &&
            void copyText(picked, t("detail.copied"), t("detail.copyFailed"))
          }
        >
          {t("detail.peers.copy")}
        </Button>
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="ghost" size="sm">
              {t("detail.peers.flagsLegend")}
            </Button>
          </PopoverTrigger>
          <PopoverContent>
            <dl className="grid grid-cols-[auto_1fr] gap-x-3">
              {PEER_FLAG_LEGEND.map(([flag, key]) => (
                <div key={flag} className="contents">
                  <dt className="font-mono">{flag}</dt>
                  <dd>{t(key)}</dd>
                </div>
              ))}
            </dl>
          </PopoverContent>
        </Popover>
      </div>
      {query.isError ? (
        <p role="alert">{t("detail.loadError")}</p>
      ) : (
        <table className="w-full text-left">
          <thead>
            <tr className="text-muted-foreground">
              <th>{t("detail.peers.country")}</th>
              <th>{t("detail.peers.address")}</th>
              <th>{t("detail.peers.client")}</th>
              <th>{t("detail.peers.flags")}</th>
              <th className="text-right">{t("detail.peers.progress")}</th>
              <th className="text-right">{t("detail.peers.down")}</th>
              <th className="text-right">{t("detail.peers.up")}</th>
            </tr>
          </thead>
          <tbody>
            {peers.map((peer: Peer) => (
              <tr
                key={peer.address}
                tabIndex={0}
                aria-selected={picked === peer.address}
                className={picked === peer.address ? "bg-muted" : undefined}
                onClick={() => setPicked(peer.address)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" || event.key === " ") {
                    event.preventDefault();
                    setPicked(peer.address);
                  }
                }}
              >
                <td>{peer.country ?? missing}</td>
                <td className="font-mono">{peer.address}</td>
                <td className="max-w-0 truncate" title={peer.client ?? ""}>
                  {peer.client ?? missing}
                </td>
                <td className="font-mono">{peer.flags ?? missing}</td>
                <td className="text-right tabular-nums">
                  {formatPercent(peer.progress, locale)}
                </td>
                <td className="text-right tabular-nums">
                  {formatRate(peer.download_rate, locale)}
                </td>
                <td className="text-right tabular-nums">
                  {formatRate(peer.upload_rate, locale)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

/** Mirrors the PATCH one-concept rule on the cached listing so the optimistic
 *  tree never shows a checkbox and a priority that disagree (doc 05 §5.8). */
function applyFileChanges(
  files: TaskFile[],
  changes: FileChange[],
): TaskFile[] {
  const byIndex = new Map(changes.map((change) => [change.index, change]));
  return files.map((file) => {
    const change = byIndex.get(file.index);
    if (!change) return file;
    const next = { ...file };
    if (change.priority !== undefined) {
      next.priority = change.priority;
      next.selected = change.priority !== "skip";
    }
    if (change.selected !== undefined) {
      next.selected = change.selected;
      if (next.priority !== null)
        next.priority = change.selected
          ? next.priority === "skip"
            ? "normal"
            : next.priority
          : "skip";
    }
    return next;
  });
}

function FilesPanel({ task }: { task: Task }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [filterInput, setFilterInput] = useState("");
  const [filter, setFilter] = useState("");
  useEffect(() => {
    const timer = setTimeout(() => setFilter(filterInput), FILTER_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [filterInput]);
  const query = useQuery({
    queryKey: [...filesKey(task.id), task.updated_at],
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => {
      const { data, error } = await api.GET("/tasks/{id}/files", {
        params: { path: { id: task.id } },
        signal,
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
      return data.files ?? [];
    },
  });
  const free = useQuery({
    queryKey: ["fs-free-space", task.destination],
    queryFn: async () => {
      const { data, error } = await api.GET("/fs/free-space", {
        params: { query: { path: task.destination } },
      });
      if (error || !data) throw new Error(t("shell.networkError"));
      return data;
    },
  });
  const files = query.data ?? [];
  const nodes = useMemo(
    () =>
      buildTree(
        files.map((file) => ({
          index: file.index,
          path: file.path,
          size_bytes: file.size_bytes,
          selected: file.selected,
          priority: file.priority as FilePriority | null,
          progress: file.progress,
        })),
      ),
    [files],
  );
  // Doc 09 §6: read-only for a single-file HTTP or FTP task — nothing to
  // select inside a one-file non-BitTorrent download.
  const readOnly = files.length <= 1 && !BITTORRENT_KINDS.has(task.source_kind);
  const mutation = useMutation({
    mutationFn: async (changes: FileChange[]) => {
      const { data, error } = await api.PATCH("/tasks/{id}/files", {
        params: { path: { id: task.id } },
        body: { files: changes },
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? t("shell.networkError"));
      return data;
    },
    onMutate: async (changes) => {
      await queryClient.cancelQueries({ queryKey: filesKey(task.id) });
      const previous = queryClient.getQueriesData<FilesBody>({
        queryKey: filesKey(task.id),
      });
      queryClient.setQueriesData<FilesBody>(
        { queryKey: filesKey(task.id) },
        (old) =>
          old ? { files: applyFileChanges(old.files ?? [], changes) } : old,
      );
      return { previous };
    },
    onError: (error, _changes, context) => {
      for (const [key, value] of context?.previous ?? [])
        queryClient.setQueryData(key, value);
      toast.error(t("shell.actionFailed", { detail: error.message }));
    },
    onSettled: () =>
      queryClient.invalidateQueries({ queryKey: filesKey(task.id) }),
  });
  return (
    <div className="flex h-full min-h-0 flex-col gap-1">
      <div className="relative w-64">
        <Input
          aria-label={t("detail.files.filter")}
          placeholder={t("detail.files.filter")}
          value={filterInput}
          onChange={(event) => setFilterInput(event.target.value)}
        />
        {filterInput !== "" ? (
          <button
            type="button"
            aria-label={t("detail.files.clearFilter")}
            className="absolute top-1/2 right-2 -translate-y-1/2"
            onClick={() => setFilterInput("")}
          >
            ✕
          </button>
        ) : null}
      </div>
      {query.isError ? (
        <p role="alert">{t("detail.loadError")}</p>
      ) : (
        <div className="min-h-0 flex-1">
          <FileTree
            nodes={nodes}
            readOnly={readOnly}
            filter={filter}
            freeBytes={free.data?.free_bytes ?? null}
            onChange={(changes) => mutation.mutate(changes)}
          />
        </div>
      )}
    </div>
  );
}

function LogPanel({ task }: { task: Task }) {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const now = useRef(new Date());
  const query = useQuery({
    queryKey: [...eventsKey(task.id), task.updated_at],
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => {
      const items: TaskEvent[] = [];
      const seen = new Set<string>();
      const eventIds = new Set<string>();
      let cursor: string | undefined;
      // Cap and dedupe the cursor walk: a server bug echoing a cursor must
      // neither loop forever nor re-render the same page's rows.
      do {
        const { data, error } = await api.GET("/tasks/{id}/events", {
          params: {
            path: { id: task.id },
            query: { limit: EVENTS_PAGE, cursor },
          },
          signal,
        });
        if (error || !data)
          throw new Error(problemDetail(error) ?? t("shell.networkError"));
        for (const item of data.items ?? []) {
          if (eventIds.has(item.id)) continue;
          eventIds.add(item.id);
          items.push(item);
        }
        cursor = data.next_cursor ?? undefined;
        if (cursor !== undefined) {
          if (seen.has(cursor) || seen.size >= MAX_EVENT_PAGES) break;
          seen.add(cursor);
        }
      } while (cursor);
      return items;
    },
  });
  const events = query.data ?? [];
  return (
    <div className="flex flex-col gap-1 text-sm">
      <div>
        <Button
          variant="ghost"
          size="sm"
          disabled={events.length === 0}
          onClick={() =>
            void copyText(
              events
                .map(
                  (event) =>
                    `${event.at} ${event.level} ${event.code} ${event.message}`,
                )
                .join("\n"),
              t("detail.copied"),
              t("detail.copyFailed"),
            )
          }
        >
          {t("detail.log.copyAll")}
        </Button>
      </div>
      {query.isError ? (
        <p role="alert">{t("detail.loadError")}</p>
      ) : events.length === 0 ? (
        <p className="text-muted-foreground">{t("empty.logs")}</p>
      ) : (
        <table className="w-full text-left">
          <thead>
            <tr className="text-muted-foreground">
              <th>{t("detail.log.time")}</th>
              <th>{t("detail.log.level")}</th>
              <th>{t("detail.log.code")}</th>
              <th>{t("detail.log.message")}</th>
            </tr>
          </thead>
          <tbody>
            {events.map((event) => (
              <tr key={event.id}>
                <td
                  className="whitespace-nowrap tabular-nums"
                  title={formatWhen(event.at, now.current, locale)}
                >
                  {formatAbsolute(event.at, locale)}
                </td>
                <td>
                  <span
                    className="rounded px-1 text-xs"
                    style={{
                      background:
                        event.level === "error"
                          ? "var(--error)"
                          : event.level === "warn"
                            ? "var(--warn)"
                            : "var(--bg-muted, var(--muted))",
                    }}
                  >
                    {event.level}
                  </span>
                </td>
                <td className="font-mono">{event.code}</td>
                <td className="max-w-0 truncate" title={event.message}>
                  {event.message}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

/** A horizontal drag handle that sizes the pane between 160 px and 70 % of
 *  the viewport; the settled height is persisted in the prefs document. */
function ResizeHandle({
  height,
  onLive,
  onCommit,
}: {
  height: number;
  /** null hands the pane back to the persisted preference. */
  onLive: (height: number | null) => void;
  onCommit: (height: number) => void;
}) {
  const { t } = useTranslation();
  const drag = useRef<{ y: number; height: number } | null>(null);
  // Mid-gesture window listeners are tracked so unmount can detach them.
  const teardownRef = useRef<(() => void) | null>(null);
  useEffect(
    () => () => {
      teardownRef.current?.();
    },
    [],
  );
  const max = () => Math.floor(window.innerHeight * 0.7);
  const clamp = (value: number) => Math.max(MIN_HEIGHT, Math.min(max(), value));
  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    // Keep mouse drags from selecting page text.
    event.preventDefault();
    // preventDefault also cancels implicit focus; restore it so arrow keys
    // keep resizing right after a pointer drag.
    event.currentTarget.focus();
    drag.current = { y: event.clientY, height };
    // No writes mid-gesture (doc 09 §3.3); pointer-up flushes once.
    useUiPrefs.getState().setDragging(true);
    const move = (moveEvent: globalThis.PointerEvent) => {
      if (drag.current)
        onLive(clamp(drag.current.height + drag.current.y - moveEvent.clientY));
    };
    const teardown = (upEvent?: globalThis.PointerEvent) => {
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", up);
      window.removeEventListener("pointercancel", cancel);
      if (drag.current && upEvent)
        onCommit(clamp(drag.current.height + drag.current.y - upEvent.clientY));
      drag.current = null;
      teardownRef.current = null;
      useUiPrefs.getState().setDragging(false);
    };
    const up = (upEvent: globalThis.PointerEvent) => teardown(upEvent);
    const cancel = () => {
      // A canceled gesture hands the height back to the persisted pref.
      onLive(null);
      teardown();
    };
    teardownRef.current = () => teardown();
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", up);
    window.addEventListener("pointercancel", cancel);
  };
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const step = event.key === "PageUp" || event.key === "PageDown" ? 64 : 16;
    const next =
      event.key === "ArrowUp"
        ? height + step
        : event.key === "ArrowDown"
          ? height - step
          : event.key === "PageUp"
            ? height + step
            : event.key === "PageDown"
              ? height - step
              : event.key === "Home"
                ? MIN_HEIGHT
                : event.key === "End"
                  ? max()
                  : null;
    if (next === null) return;
    event.preventDefault();
    onCommit(clamp(next));
  };
  return (
    <div
      role="separator"
      aria-orientation="horizontal"
      aria-label={t("detail.resize")}
      aria-valuenow={Math.round(height)}
      aria-valuemin={MIN_HEIGHT}
      aria-valuemax={max()}
      tabIndex={0}
      onPointerDown={onPointerDown}
      onKeyDown={onKeyDown}
      // touch-action: none keeps touch drags from firing pointercancel.
      className="h-1.5 shrink-0 cursor-row-resize outline-none [touch-action:none] hover:bg-accent focus-visible:bg-accent"
    />
  );
}

export function DetailPane(): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const selection = useTasks((state) => state.selection);
  const tasks = useTasks((state) => state.tasks);
  const storedHeight = useUiPrefs((state) => state.detailHeight);
  const storedTab = useUiPrefs((state) => state.detailTab);
  const [liveHeight, setLiveHeight] = useState<number | null>(null);
  const selected = [...selection].flatMap((id) => {
    const task = tasks.get(id);
    return task ? [task] : [];
  });
  if (selected.length === 0) {
    return (
      <section
        aria-label={t("detail.region")}
        className="border-t border-border px-2 py-1.5 text-center text-sm text-muted-foreground"
      >
        {t("detail.empty")} · {t("detail.emptyHint")}
      </section>
    );
  }
  if (selected.length > 1) {
    const size = selected.reduce(
      (sum, task) => sum + (task.total_bytes ?? 0),
      0,
    );
    const rate = selected.reduce((sum, task) => sum + task.download_rate, 0);
    return (
      <section
        aria-label={t("detail.region")}
        className="border-t border-border px-2 py-1.5 text-sm text-muted-foreground"
      >
        {t("detail.aggregate", {
          count: selected.length,
          size: formatBytes(size, locale),
          rate: formatRate(rate, locale),
        })}
      </section>
    );
  }
  const task = selected[0];
  const tabs = visibleTabs(task);
  const activeTab = tabs.includes(storedTab as DetailTab)
    ? (storedTab as DetailTab)
    : "general";
  const height = Math.max(
    MIN_HEIGHT,
    Math.min(Math.floor(window.innerHeight * 0.7), liveHeight ?? storedHeight),
  );
  return (
    <section
      aria-label={t("detail.region")}
      className="flex shrink-0 flex-col border-t border-border"
      style={{ height }}
    >
      <ResizeHandle
        height={height}
        onLive={setLiveHeight}
        onCommit={(value) => {
          // Clearing live height lets the persisted pref own the pane again.
          setLiveHeight(null);
          useUiPrefs.getState().patch({ detailHeight: value });
        }}
      />
      <Tabs
        value={activeTab}
        onValueChange={(value) =>
          useUiPrefs.getState().patch({ detailTab: value })
        }
        className="min-h-0 flex-1"
      >
        <TabsList variant="line" className="justify-start">
          {tabs.map((tab) => (
            <TabsTrigger key={tab} value={tab}>
              {t(`detail.tabs.${tab}`)}
            </TabsTrigger>
          ))}
        </TabsList>
        <TabsContent value="general" className="overflow-auto p-2">
          <GeneralPanel task={task} />
        </TabsContent>
        <TabsContent value="transfer" className="overflow-auto p-2">
          <TransferPanel task={task} />
        </TabsContent>
        {BITTORRENT_KINDS.has(task.source_kind) ? (
          <>
            <TabsContent value="trackers" className="overflow-auto p-2">
              <TrackersPanel task={task} />
            </TabsContent>
            <TabsContent value="peers" className="overflow-auto p-2">
              <PeersPanel task={task} />
            </TabsContent>
          </>
        ) : null}
        <TabsContent value="files" className="overflow-hidden p-2">
          <FilesPanel task={task} />
        </TabsContent>
        <TabsContent value="log" className="overflow-auto p-2">
          <LogPanel task={task} />
        </TabsContent>
      </Tabs>
    </section>
  );
}
