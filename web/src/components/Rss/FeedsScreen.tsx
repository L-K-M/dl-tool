import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type FormEvent,
  type JSX,
} from "react";
import {
  useQueries,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Rss } from "lucide-react";

import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { cn } from "../../lib/utils";
import {
  formatAbsolute,
  formatBytes,
  formatInteger,
  formatWhen,
} from "../../lib/format";
import strings from "../../locales/en/rss.json";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "../ui/context-menu";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

initI18n().addResourceBundle("en", "rss", strings);

export type FeedState = "ok" | "loading" | "error";

export interface FeedRow {
  id: string; // fed_…
  url: string;
  title: string | null;
  enabled: boolean;
  unread_count: number;
  last_error: string | null;
  state: FeedState; // 'error' when last_error is set, 'loading' while a refresh is in flight
}

export interface ItemRow {
  id: string; // itm_…
  feed_id: string;
  title: string;
  link: string | null;
  download_url: string;
  info_hash: string | null;
  size_bytes: number | null;
  published_at: string | null; // RFC 3339
  read: boolean;
  matched_rules: { id: string; name: string }[];
}

type FeedDTO = components["schemas"]["FeedDTO"];
type FeedItemDTO = components["schemas"]["FeedItemDTO"];
type ItemsPage = {
  data?: components["schemas"]["ListFeedItemsOutputBody"];
  error?: components["schemas"]["ErrorModel"];
  response: Response;
};

const PAGE_SIZE = 500;
const CONFLICT = 409;
const feedsKey = ["rss-feeds"] as const;
const itemsKey = "rss-items";
const missing = "—";
/** Only http(s) links may be opened from feed-provided data. */
const HTTP_URL = /^https?:\/\//i;
const GLYPHS: Record<FeedState, string> = {
  ok: "●",
  loading: "◐",
  error: "⚠",
};
// The Add-feed dialog's Folder is a client-side grouping only — the schema
// carries no folder concept (task out-of-scope note), so the map persists in
// localStorage, keyed by feed id.
const FOLDER_KEY = "dl.rss.folders.v1";

function readFolders(): Record<string, string> {
  try {
    const raw = localStorage.getItem(FOLDER_KEY);
    if (raw === null) return {};
    const parsed = JSON.parse(raw) as unknown;
    if (parsed === null || typeof parsed !== "object") return {};
    return parsed as Record<string, string>;
  } catch {
    return {};
  }
}

function storeFolders(map: Record<string, string>): void {
  try {
    localStorage.setItem(FOLDER_KEY, JSON.stringify(map));
  } catch {
    // best-effort; grouping is a convenience, not state that must survive
  }
}

function toItemRow(item: FeedItemDTO): ItemRow {
  return {
    id: item.id,
    feed_id: item.feed_id,
    title: item.title,
    link: item.link,
    download_url: item.download_url ?? "",
    info_hash: item.info_hash,
    size_bytes: item.size_bytes,
    published_at: item.published_at,
    read: item.read,
    matched_rules: item.matched_rules ?? [],
  };
}

/** The feed list, the two refresh actions and the list query's error state.
 *  Exported for the test and for T073's reuse of the feed list; isError and
 *  refetch extend the task's contract so an errored list never renders as
 *  the empty state. */
export function useFeeds(): {
  feeds: FeedRow[];
  refresh: (id: string) => Promise<void>;
  refreshAll: () => Promise<void>;
  isError: boolean;
  refetch: () => void;
} {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  // Ids with a POST /feeds/{id}/refresh in flight; they render ◐ until the
  // call settles, whatever the feed's stored state is.
  const [refreshing, setRefreshing] = useState<ReadonlySet<string>>(new Set());
  const query = useQuery({
    queryKey: feedsKey,
    queryFn: async (): Promise<FeedDTO[]> => {
      const { data, error } = await api.GET("/feeds");
      if (!data)
        throw new Error(
          error?.detail ?? error?.title ?? t("shell.networkError"),
        );
      return data.feeds ?? [];
    },
  });

  const refresh = useCallback(
    async (id: string) => {
      setRefreshing((prev) => new Set(prev).add(id));
      try {
        const { data, error } = await api
          .POST("/feeds/{id}/refresh", {
            params: { path: { id } },
          })
          .catch(() => ({ data: undefined, error: undefined }));
        if (!data) {
          toast.error(
            t("rss:toast.refreshFailed", {
              detail: error?.detail ?? error?.title ?? t("shell.networkError"),
            }),
          );
        } else if (data.error) {
          // A fetch failure is still 200 with error set (doc 05 §10.1).
          toast.error(t("rss:toast.refreshFailed", { detail: data.error }));
        } else {
          toast.success(t("rss:toast.itemsAdded", { count: data.items_added }));
        }
      } finally {
        setRefreshing((prev) => {
          const next = new Set(prev);
          next.delete(id);
          return next;
        });
        await queryClient.invalidateQueries({ queryKey: feedsKey });
        // Item queries are per feed; one refresh refetches only its own list.
        await queryClient.invalidateQueries({
          queryKey: [itemsKey, id],
        });
      }
    },
    [queryClient, t],
  );

  const refreshAll = useCallback(async () => {
    // Sequential, so the toasts land in feed order and the poll ladder sees
    // one forced fetch at a time.
    for (const feed of query.data ?? []) await refresh(feed.id);
  }, [query.data, refresh]);

  const feeds = useMemo<FeedRow[]>(
    () =>
      (query.data ?? []).map((feed) => ({
        id: feed.id,
        url: feed.url,
        title: feed.title,
        enabled: feed.enabled,
        unread_count: feed.unread_count,
        last_error: feed.last_error,
        state: refreshing.has(feed.id)
          ? "loading"
          : feed.last_error !== null
            ? "error"
            : "ok",
      })),
    [query.data, refreshing],
  );

  return {
    feeds,
    refresh,
    refreshAll,
    isError: query.isError,
    refetch: () => void query.refetch(),
  };
}

/** Every stored item of the named feeds, newest first. The endpoint is
 *  per-feed and cursor-paginated, so each feed gets its own query (one
 *  refresh then refetches only that feed's pages) and the hook merges the
 *  results. */
function useFeedItems(feedIds: string[], unreadOnly: boolean) {
  const { t } = useTranslation();
  // combine is memoized via structural sharing, so the merged list keeps a
  // stable identity across renders — a plain `useQueries` result array is
  // rebuilt every render and would churn every downstream memo.
  const combine = useCallback(
    (results: UseQueryResult<ItemRow[], Error>[]) => ({
      items: results
        .flatMap((result) => result.data ?? [])
        // Date.parse keeps mixed RFC 3339 offset styles chronological; NaN
        // (a missing or invalid timestamp) sorts last via `|| 0`.
        .sort(
          (a, b) =>
            (Date.parse(b.published_at ?? "") || 0) -
            (Date.parse(a.published_at ?? "") || 0),
        ),
      isPending: results.some((result) => result.isPending),
      error: results.find((result) => result.error)?.error ?? null,
    }),
    [],
  );
  return useQueries({
    queries: feedIds.map((feedId) => ({
      queryKey: [itemsKey, feedId, unreadOnly] as const,
      queryFn: async ({ signal }): Promise<ItemRow[]> => {
        const rows: ItemRow[] = [];
        let cursor: string | null = null;
        do {
          // An explicit annotation keeps the cursor-walking loop out of the
          // request's own type inference (TS7022).
          const page: ItemsPage = await api.GET("/feeds/{id}/items", {
            params: {
              path: { id: feedId },
              query: {
                limit: PAGE_SIZE,
                cursor: cursor ?? undefined,
                unread: unreadOnly || undefined,
              },
            },
            signal,
          });
          if (!page.data)
            throw new Error(
              page.error?.detail ??
                page.error?.title ??
                t("shell.networkError"),
            );
          rows.push(...(page.data.items ?? []).map(toItemRow));
          cursor = page.data.next_cursor;
        } while (cursor !== null);
        return rows;
      },
    })),
    combine,
  });
}

/** One-line text dialog for the context menu's Rename… and Edit URL… entries. */
function FeedFieldDialog({
  title,
  label,
  initial,
  onClose,
  onSubmit,
}: {
  title: string;
  label: string;
  initial: string;
  onClose: () => void;
  onSubmit: (value: string) => Promise<string | null>;
}): JSX.Element {
  const { t } = useTranslation();
  const [value, setValue] = useState(initial);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    // Enter can re-fire while the submit button is mid-request.
    if (busy) return;
    setBusy(true);
    const problem = await onSubmit(value.trim());
    setBusy(false);
    if (problem === null) onClose();
    else setError(problem);
  };
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <form onSubmit={(event) => void submit(event)} className="space-y-3">
          <div className="space-y-1">
            <Label htmlFor="feed-field">{label}</Label>
            <Input
              id="feed-field"
              value={value}
              onChange={(event) => setValue(event.target.value)}
            />
            {error !== null && (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("rss:feeds.dialog.cancel")}
            </Button>
            <Button type="submit" disabled={busy || value.trim() === ""}>
              {t("rss:feeds.dialog.save")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function FeedsScreen(): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const queryClient = useQueryClient();
  const { feeds, refresh, refreshAll, isError, refetch } = useFeeds();
  const [selectedFeedId, setSelectedFeedId] = useState<string | null>(null);
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [focusId, setFocusId] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [unreadOnly, setUnreadOnly] = useState(false);
  const [folders, setFolders] = useState<Record<string, string>>(readFolders);
  const [addOpen, setAddOpen] = useState(false);
  const [addUrl, setAddUrl] = useState("");
  const [addName, setAddName] = useState("");
  const [addFolder, setAddFolder] = useState("");
  const [addAuto, setAddAuto] = useState(false);
  const [addError, setAddError] = useState<string | null>(null);
  const [addBusy, setAddBusy] = useState(false);
  const [renameFeed, setRenameFeed] = useState<FeedRow | null>(null);
  const [editUrlFeed, setEditUrlFeed] = useState<FeedRow | null>(null);
  const [removeFeed, setRemoveFeed] = useState<FeedRow | null>(null);

  // A removed or filtered-away feed must not keep owning the item table.
  const activeFeedId = feeds.some((feed) => feed.id === selectedFeedId)
    ? selectedFeedId
    : null;
  const itemFeedIds = useMemo(
    () => (activeFeedId !== null ? [activeFeedId] : feeds.map((f) => f.id)),
    [activeFeedId, feeds],
  );
  const itemsResult = useFeedItems(itemFeedIds, unreadOnly);
  const feedNames = useMemo(
    () => new Map(feeds.map((feed) => [feed.id, feed.title ?? feed.url])),
    [feeds],
  );
  const items = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    if (needle === "") return itemsResult.items;
    return itemsResult.items.filter((item) =>
      item.title.toLowerCase().includes(needle),
    );
  }, [itemsResult.items, filter]);
  const byId = useMemo(
    () => new Map(itemsResult.items.map((item) => [item.id, item])),
    [itemsResult.items],
  );
  const focused = focusId !== null ? (byId.get(focusId) ?? null) : null;

  // Items that leave the loaded set (feed switch, unread filter, refresh)
  // must not stay selected — the toolbar would keep offering no-op actions.
  useEffect(() => {
    setSelected((prev) => {
      const next = new Set([...prev].filter((id) => byId.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [byId]);

  const invalidateAll = useCallback(async () => {
    await queryClient.invalidateQueries({ queryKey: feedsKey });
    await queryClient.invalidateQueries({ queryKey: [itemsKey] });
  }, [queryClient]);

  const fail = useCallback(
    (detail: string | undefined) =>
      toast.error(
        t("rss:toast.requestFailed", {
          detail: detail ?? t("shell.networkError"),
        }),
      ),
    [t],
  );

  const markRead = useCallback(
    async (ids: string[]) => {
      const groups = new Map<string, string[]>();
      for (const id of ids) {
        const item = byId.get(id);
        if (item === undefined) continue;
        groups.set(item.feed_id, [...(groups.get(item.feed_id) ?? []), id]);
      }
      for (const [feedId, feedIds] of groups) {
        try {
          const { error } = await api.PATCH("/feeds/{id}/items", {
            params: { path: { id: feedId } },
            body: { ids: feedIds, read: true },
          });
          if (error) fail(error.detail ?? error.title);
        } catch {
          fail(undefined);
        }
        await queryClient.invalidateQueries({
          queryKey: [itemsKey, feedId],
        });
      }
      await queryClient.invalidateQueries({ queryKey: feedsKey });
    },
    [byId, fail, queryClient],
  );

  /** POST /feeds/{id}/items/read-all for one feed; shared by the toolbar's
   *  "Mark all read" and the feed context menu. */
  const markFeedAllRead = useCallback(
    async (feedId: string) => {
      try {
        const { error } = await api.POST("/feeds/{id}/items/read-all", {
          params: { path: { id: feedId } },
        });
        if (error) fail(error.detail ?? error.title);
      } catch {
        fail(undefined);
      }
      await queryClient.invalidateQueries({
        queryKey: [itemsKey, feedId],
      });
      await queryClient.invalidateQueries({ queryKey: feedsKey });
    },
    [fail, queryClient],
  );

  const markAllRead = useCallback(async () => {
    for (const feedId of itemFeedIds) await markFeedAllRead(feedId);
  }, [itemFeedIds, markFeedAllRead]);

  /** POST /tasks with the items' download_url values. One toast per rejected
   *  URI; on success only the created-count toast (task step 6). */
  const downloadItems = useCallback(
    async (list: ItemRow[]) => {
      const uris = list
        .map((item) => item.download_url)
        .filter((uri) => uri !== "");
      if (uris.length < list.length)
        toast.info(
          t("rss:toast.itemsSkipped", { count: list.length - uris.length }),
        );
      if (uris.length === 0) return;
      const { data, error } = await api
        .POST("/tasks", { body: { uris } })
        .catch(() => ({ data: undefined, error: undefined }));
      if (!data) {
        fail(error?.detail ?? error?.title);
        return;
      }
      const rejected = data.rejected ?? [];
      for (const entry of rejected) {
        const item = list.find((it) => it.download_url === entry.uri);
        toast.error(
          t("rss:toast.itemRejected", {
            title: item?.title ?? entry.uri ?? "",
            detail: entry.detail !== "" ? entry.detail : entry.type,
          }),
        );
      }
      const created = data.created?.length ?? 0;
      if (created > 0)
        toast.success(t("rss:toast.tasksCreated", { count: created }));
      const rejectedUris = new Set(rejected.map((entry) => entry.uri));
      setSelected((prev) => {
        const next = new Set(prev);
        for (const item of list)
          if (!rejectedUris.has(item.download_url)) next.delete(item.id);
        return next;
      });
    },
    [fail, t],
  );

  const downloadSelected = useCallback(() => {
    const list = [...selected]
      .map((id) => byId.get(id))
      .filter((item): item is ItemRow => item !== undefined);
    void downloadItems(list);
  }, [selected, byId, downloadItems]);

  const copyFeedUrl = useCallback(
    async (feed: FeedRow) => {
      try {
        await navigator.clipboard.writeText(feed.url);
        toast.success(t("rss:toast.copied"));
      } catch {
        fail(undefined);
      }
    },
    [fail, t],
  );

  const deleteFeed = useCallback(
    async (feed: FeedRow) => {
      let error: { detail?: string; title?: string } | undefined;
      let status = 0;
      try {
        const out = await api.DELETE("/feeds/{id}", {
          params: { path: { id: feed.id } },
        });
        error = out.error;
        status = out.response.status;
      } catch {
        fail(undefined);
        return;
      }
      // 204 and an already-gone 404 both leave the feed deleted.
      if (error && status !== 404) {
        fail(error.detail ?? error.title);
        return;
      }
      setRemoveFeed(null);
      if (selectedFeedId === feed.id) setSelectedFeedId(null);
      // Drop the feed's folder assignment so the localStorage map cannot
      // accumulate orphaned ids across add/remove cycles. The write lives
      // outside the state updater, which must stay pure.
      if (feed.id in folders) {
        const next = { ...folders };
        delete next[feed.id];
        storeFolders(next);
        setFolders(next);
      }
      await invalidateAll();
    },
    [fail, folders, invalidateAll, selectedFeedId],
  );

  const patchFeed = useCallback(
    async (
      feed: FeedRow,
      body: { title?: string; url?: string },
    ): Promise<string | null> => {
      try {
        const { error } = await api.PATCH("/feeds/{id}", {
          params: { path: { id: feed.id } },
          body,
        });
        if (error)
          return error.detail ?? error.title ?? t("shell.networkError");
      } catch {
        return t("shell.networkError");
      }
      await queryClient.invalidateQueries({ queryKey: feedsKey });
      return null;
    },
    [queryClient, t],
  );

  const submitAdd = useCallback(
    async (event: FormEvent) => {
      event.preventDefault();
      if (addBusy) return;
      setAddBusy(true);
      setAddError(null);
      let data: FeedDTO | undefined;
      let problem: { detail?: string; title?: string } | undefined;
      let status = 0;
      try {
        const out = await api.POST("/feeds", {
          body: {
            url: addUrl.trim(),
            title: addName.trim() === "" ? undefined : addName.trim(),
            auto_download: addAuto || undefined,
          },
        });
        data = out.data;
        problem = out.error;
        status = out.response.status;
      } catch {
        setAddBusy(false);
        setAddError(t("shell.networkError"));
        return;
      }
      setAddBusy(false);
      if (!data) {
        setAddError(
          status === CONFLICT
            ? t("rss:feeds.dialog.duplicate")
            : (problem?.detail ?? problem?.title ?? t("shell.networkError")),
        );
        return;
      }
      const folder = addFolder.trim();
      if (folder !== "") {
        setFolders((prev) => {
          const next = { ...prev, [data.id]: folder };
          storeFolders(next);
          return next;
        });
      }
      setAddOpen(false);
      setAddUrl("");
      setAddName("");
      setAddFolder("");
      setAddAuto(false);
      await queryClient.invalidateQueries({ queryKey: feedsKey });
    },
    [addUrl, addName, addFolder, addAuto, addBusy, queryClient, t],
  );

  const toggleItem = useCallback((id: string, on: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const visibleIds = useMemo(() => items.map((item) => item.id), [items]);
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  const toggleAll = useCallback(
    (on: boolean) => {
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of visibleIds) {
          if (on) next.add(id);
          else next.delete(id);
        }
        return next;
      });
    },
    [visibleIds],
  );

  // Client-side folder grouping: feeds under the same Add-dialog folder sit
  // under one static heading; the folder never leaves the browser.
  const feedGroups = useMemo(() => {
    const groups = new Map<string, FeedRow[]>();
    for (const feed of feeds) {
      const folder = folders[feed.id] ?? "";
      groups.set(folder, [...(groups.get(folder) ?? []), feed]);
    }
    return [...groups.entries()];
  }, [feeds, folders]);

  const renderFeed = (feed: FeedRow) => (
    <ContextMenu key={feed.id}>
      <ContextMenuTrigger asChild>
        <button
          type="button"
          aria-pressed={activeFeedId === feed.id}
          title={feed.last_error ?? feed.url}
          onClick={() =>
            setSelectedFeedId((prev) => (prev === feed.id ? null : feed.id))
          }
          className={cn(
            "flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-muted",
            activeFeedId === feed.id && "bg-accent text-accent-foreground",
          )}
        >
          <span
            aria-hidden="true"
            className={cn(feed.state === "error" && "text-destructive")}
          >
            {GLYPHS[feed.state]}
          </span>
          <span className="truncate">{feed.title ?? feed.url}</span>
          {feed.last_error !== null ? (
            <span className="count ml-auto rounded-full bg-muted px-1.5 text-xs text-destructive">
              {"!"}
            </span>
          ) : (
            feed.unread_count > 0 && (
              <span className="count ml-auto rounded-full bg-muted px-1.5 text-xs text-muted-foreground">
                {formatInteger(feed.unread_count, locale)}
              </span>
            )
          )}
        </button>
      </ContextMenuTrigger>
      <ContextMenuContent>
        <ContextMenuItem onSelect={() => void refresh(feed.id)}>
          {t("rss:feeds.menu.update")}
        </ContextMenuItem>
        <ContextMenuItem onSelect={() => setRenameFeed(feed)}>
          {t("rss:feeds.menu.rename")}
        </ContextMenuItem>
        <ContextMenuItem onSelect={() => setEditUrlFeed(feed)}>
          {t("rss:feeds.menu.editUrl")}
        </ContextMenuItem>
        <ContextMenuSeparator />
        <ContextMenuItem onSelect={() => void markFeedAllRead(feed.id)}>
          {t("rss:feeds.menu.markAllRead")}
        </ContextMenuItem>
        <ContextMenuItem onSelect={() => void copyFeedUrl(feed)}>
          {t("rss:feeds.menu.copyUrl")}
        </ContextMenuItem>
        <ContextMenuSeparator />
        <ContextMenuItem
          variant="destructive"
          onSelect={() => setRemoveFeed(feed)}
        >
          {t("rss:feeds.menu.remove")}
        </ContextMenuItem>
      </ContextMenuContent>
    </ContextMenu>
  );

  return (
    <div className="flex h-full min-h-0" data-testid="feeds-screen">
      <div className="flex w-60 shrink-0 flex-col border-r">
        <div className="flex items-center justify-between gap-2 p-2">
          <h2 className="text-sm font-semibold">{t("rss:feeds.listLabel")}</h2>
          <Button size="sm" variant="outline" onClick={() => setAddOpen(true)}>
            {t("rss:feeds.addFeed")}
          </Button>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-1">
          {isError ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 p-4 text-center">
              <p role="alert" className="text-sm text-destructive">
                {t("rss:feeds.loadError")}
              </p>
              <Button size="sm" variant="outline" onClick={() => refetch()}>
                {t("rss:feeds.retry")}
              </Button>
            </div>
          ) : feeds.length === 0 ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 p-4 text-center">
              <Rss
                aria-hidden="true"
                className="size-8 text-muted-foreground"
              />
              <p className="text-sm text-muted-foreground">
                {t("empty.feeds")}
              </p>
              <Button size="sm" onClick={() => setAddOpen(true)}>
                {t("rss:feeds.addFeed")}
              </Button>
            </div>
          ) : (
            feedGroups.map(([folder, group]) => (
              <div key={folder === "" ? "ungrouped" : folder}>
                {folder !== "" && (
                  <div className="px-2 py-1 text-xs font-semibold uppercase text-muted-foreground">
                    {folder}
                  </div>
                )}
                {group.map(renderFeed)}
              </div>
            ))
          )}
        </div>
        <div className="flex gap-2 border-t p-2">
          <Button
            size="sm"
            variant="outline"
            disabled={activeFeedId === null}
            onClick={() => activeFeedId !== null && void refresh(activeFeedId)}
          >
            {t("rss:feeds.update")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={feeds.length === 0}
            onClick={() => void refreshAll()}
          >
            {t("rss:feeds.updateAll")}
          </Button>
        </div>
      </div>

      <div className="flex min-w-0 flex-1 flex-col">
        <div className="flex flex-wrap items-center gap-2 border-b p-2">
          <Button
            size="sm"
            disabled={selected.size === 0}
            onClick={downloadSelected}
          >
            {t("rss:items.downloadSelected")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={selected.size === 0}
            onClick={() => void markRead([...selected])}
          >
            {t("rss:items.markRead")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={itemFeedIds.length === 0}
            onClick={() => void markAllRead()}
          >
            {t("rss:items.markAllRead")}
          </Button>
          <span className="flex items-center gap-1.5 text-sm">
            <Checkbox
              id="unread-only"
              checked={unreadOnly}
              onCheckedChange={(state) => setUnreadOnly(state === true)}
            />
            <Label htmlFor="unread-only">{t("rss:items.unreadOnly")}</Label>
          </span>
          <Input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder={t("rss:items.filterPlaceholder")}
            aria-label={t("rss:items.filterPlaceholder")}
            className="ml-auto w-48"
          />
        </div>

        <div className="min-h-0 flex-1 overflow-auto">
          {itemsResult.error !== null ? (
            <p role="alert" className="p-4 text-sm text-destructive">
              {itemsResult.error.message}
            </p>
          ) : items.length === 0 ? (
            <p className="p-4 text-sm text-muted-foreground">
              {itemsResult.isPending
                ? t("rss:items.loading")
                : filter.trim() !== ""
                  ? t("rss:items.noMatches")
                  : t("rss:items.empty")}
            </p>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b text-left text-xs text-muted-foreground">
                  <th className="w-8 px-2 py-1">
                    <Checkbox
                      checked={allSelected}
                      onCheckedChange={(state) => toggleAll(state === true)}
                      aria-label={t("rss:items.selectAll")}
                    />
                  </th>
                  <th className="px-2 py-1 font-medium">
                    {t("rss:items.colTitle")}
                  </th>
                  <th className="w-28 px-2 py-1 font-medium">
                    {t("rss:items.colFeed")}
                  </th>
                  <th className="w-20 px-2 py-1 font-medium">
                    {t("rss:items.colAge")}
                  </th>
                  <th className="w-32 px-2 py-1 font-medium">
                    {t("rss:items.colRules")}
                  </th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr
                    key={item.id}
                    tabIndex={0}
                    onClick={() => setFocusId(item.id)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        setFocusId(item.id);
                      }
                    }}
                    className={cn(
                      "cursor-pointer border-b last:border-0 hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-none",
                      focusId === item.id && "bg-accent/50",
                    )}
                  >
                    <td
                      className="px-2 py-1"
                      onClick={(event) => event.stopPropagation()}
                    >
                      <Checkbox
                        checked={selected.has(item.id)}
                        onCheckedChange={(state) =>
                          toggleItem(item.id, state === true)
                        }
                        aria-label={t("rss:items.selectItem", {
                          title: item.title,
                        })}
                      />
                    </td>
                    <td
                      className={cn(
                        "max-w-0 truncate px-2 py-1",
                        item.read ? "text-muted-foreground" : "font-medium",
                      )}
                      title={item.title}
                    >
                      <span aria-hidden="true">{item.read ? "○" : "●"}</span>{" "}
                      <span className="sr-only">
                        {item.read
                          ? t("rss:items.read")
                          : t("rss:items.unread")}
                      </span>{" "}
                      {item.title}
                    </td>
                    <td className="truncate px-2 py-1">
                      {feedNames.get(item.feed_id) ?? missing}
                    </td>
                    <td className="px-2 py-1 whitespace-nowrap">
                      {item.published_at !== null
                        ? formatWhen(item.published_at, undefined, locale)
                        : missing}
                    </td>
                    <td className="px-2 py-1">
                      <span className="flex flex-wrap gap-1">
                        {item.matched_rules.map((rule) => (
                          <span
                            key={rule.id}
                            title={rule.name}
                            className="rounded-md border px-1.5 py-0.5 text-xs"
                          >
                            {rule.name}
                          </span>
                        ))}
                        {item.matched_rules.length === 0 && missing}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        <section
          aria-label={t("rss:items.preview")}
          className="shrink-0 border-t p-3"
        >
          <h3 className="mb-1 text-xs font-semibold uppercase text-muted-foreground">
            {t("rss:items.preview")}
          </h3>
          {focused === null ? (
            <p className="text-sm text-muted-foreground">
              {t("rss:items.noSelection")}
            </p>
          ) : (
            <div className="space-y-1 text-sm">
              <p className="font-medium">{focused.title}</p>
              <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
                <dt className="text-muted-foreground">
                  {t("rss:items.published")}
                </dt>
                <dd>
                  {focused.published_at !== null
                    ? formatAbsolute(focused.published_at, locale)
                    : missing}
                </dd>
                <dt className="text-muted-foreground">{t("rss:items.size")}</dt>
                <dd>{formatBytes(focused.size_bytes, locale)}</dd>
                <dt className="text-muted-foreground">
                  {t("rss:items.downloadUrl")}
                </dt>
                <dd className="break-all">
                  {focused.download_url === "" ? missing : focused.download_url}
                </dd>
              </dl>
              <div className="flex gap-2 pt-1">
                <Button
                  size="sm"
                  disabled={focused.download_url === ""}
                  onClick={() => void downloadItems([focused])}
                >
                  {t("rss:items.download")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => void markRead([focused.id])}
                >
                  {t("rss:items.markRead")}
                </Button>
                {focused.link !== null && HTTP_URL.test(focused.link) && (
                  <Button size="sm" variant="outline" asChild>
                    <a
                      href={focused.link}
                      target="_blank"
                      rel="noopener noreferrer"
                    >
                      {t("rss:items.openLink")}
                    </a>
                  </Button>
                )}
              </div>
            </div>
          )}
        </section>
      </div>

      <Dialog
        open={addOpen}
        onOpenChange={(open) => {
          setAddOpen(open);
          if (!open) setAddError(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("rss:feeds.dialog.addTitle")}</DialogTitle>
          </DialogHeader>
          <form
            onSubmit={(event) => void submitAdd(event)}
            className="space-y-3"
          >
            <div className="space-y-1">
              <Label htmlFor="add-feed-url">{t("rss:feeds.dialog.url")}</Label>
              <Input
                id="add-feed-url"
                value={addUrl}
                onChange={(event) => setAddUrl(event.target.value)}
                placeholder={t("rss:feeds.dialog.urlPlaceholder")}
              />
              {addError !== null && (
                <p role="alert" className="text-sm text-destructive">
                  {addError}
                </p>
              )}
            </div>
            <div className="space-y-1">
              <Label htmlFor="add-feed-name">
                {t("rss:feeds.dialog.name")}
              </Label>
              <Input
                id="add-feed-name"
                value={addName}
                onChange={(event) => setAddName(event.target.value)}
              />
            </div>
            <div className="space-y-1">
              <Label htmlFor="add-feed-folder">
                {t("rss:feeds.dialog.folder")}
              </Label>
              <Input
                id="add-feed-folder"
                value={addFolder}
                onChange={(event) => setAddFolder(event.target.value)}
              />
            </div>
            <span className="flex items-center gap-2 text-sm">
              <Checkbox
                id="add-feed-auto"
                checked={addAuto}
                onCheckedChange={(state) => setAddAuto(state === true)}
              />
              <Label htmlFor="add-feed-auto">
                {t("rss:feeds.dialog.autoDownload")}
              </Label>
            </span>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => setAddOpen(false)}
              >
                {t("rss:feeds.dialog.cancel")}
              </Button>
              <Button type="submit" disabled={addBusy || addUrl.trim() === ""}>
                {t("rss:feeds.dialog.submit")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {renameFeed !== null && (
        <FeedFieldDialog
          title={t("rss:feeds.dialog.renameTitle")}
          label={t("rss:feeds.dialog.name")}
          initial={renameFeed.title ?? ""}
          onClose={() => setRenameFeed(null)}
          onSubmit={(value) => patchFeed(renameFeed, { title: value })}
        />
      )}
      {editUrlFeed !== null && (
        <FeedFieldDialog
          title={t("rss:feeds.dialog.editUrlTitle")}
          label={t("rss:feeds.dialog.url")}
          initial={editUrlFeed.url}
          onClose={() => setEditUrlFeed(null)}
          onSubmit={(value) => patchFeed(editUrlFeed, { url: value })}
        />
      )}
      {removeFeed !== null && (
        <Dialog open onOpenChange={(open) => !open && setRemoveFeed(null)}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t("rss:feeds.dialog.removeTitle")}</DialogTitle>
            </DialogHeader>
            <p className="text-sm">
              {t("rss:feeds.dialog.removeBody", {
                name: removeFeed.title ?? removeFeed.url,
              })}
            </p>
            <DialogFooter>
              <Button variant="outline" onClick={() => setRemoveFeed(null)}>
                {t("rss:feeds.dialog.cancel")}
              </Button>
              <Button
                variant="destructive"
                onClick={() => void deleteFeed(removeFeed)}
              >
                {t("rss:feeds.dialog.remove")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
