import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type JSX,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { DropdownMenu } from "radix-ui";
import { toast } from "sonner";
import { ChevronDown } from "lucide-react";

import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { formatBytes, formatInteger } from "../../lib/format";
import { useUiPrefs, type SavedSearch } from "../../store/useUiPrefs";
import {
  AddTaskDialog,
  type SearchResultOutcome,
} from "../AddTask/AddTaskDialog";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "../ui/tooltip";
import {
  ResultRowStateContext,
  ResultsGrid,
  type ResultRowState,
  type SearchResultView,
} from "./ResultsGrid";
import { SaveSearchButton, SavedSearchesMenu } from "./SavedSearches";

export interface EngineStatusView {
  id: string;
  name: string;
  status: "queued" | "searching" | "done" | "error";
  count: number;
  error: string | null;
}

type SearchJobBody = components["schemas"]["SearchJobOutputBody"];
type CreateOutput = components["schemas"]["CreateTasksOutputBody"];
type RejectedURI = components["schemas"]["RejectedURI"];

const POLL_MS = 1000;
const PAGE_SIZE = 500;
const MAX_SUBMIT_IDS = 50;
const FLASH_MS = 1200;
const SEARCH_STATE_KEY = "dl.search.v1";
const SEARCH_JOB_KEY = "dl.search.job";
const CONFLICT_TYPE = "/problems/conflict";
// A created task's rendered source_uri is the opaque display reference —
// search-result:<res_id> — which keys each created task back to the result
// row it came from, whatever order created[] arrives in.
const RESULT_DISPLAY_PREFIX = "search-result:";

const menuContentClass =
  "z-50 min-w-36 rounded-lg bg-popover p-1 text-sm text-popover-foreground shadow-md ring-1 ring-foreground/10";
const menuItemClass =
  "flex cursor-default items-center gap-1.5 rounded-md px-1.5 py-1 outline-none select-none focus:bg-accent focus:text-accent-foreground data-disabled:pointer-events-none data-disabled:opacity-50";

class HttpError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

interface SearchJobPage {
  finished: boolean;
  total: number;
  engines: EngineStatusView[];
  results: SearchResultView[];
}

/** Starts a job with POST /search, then polls GET /search/{id} every 1000 ms until
 *  finished, then stops. Stop() abandons the poll and calls DELETE /search/{id}. */
export function useSearchJob(): {
  start: (q: {
    query: string;
    indexer_ids: string[];
    categories: number[];
  }) => Promise<void>;
  stop: () => Promise<void>;
  jobId: string | null;
  starting: boolean;
  finished: boolean;
  total: number;
  engines: EngineStatusView[];
  results: SearchResultView[];
  error: string | null;
} {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  // The job id survives navigation within the session (doc 09 §7), so a
  // remounted screen resumes polling the same job instead of losing it.
  const [jobId, setJobId] = useState<string | null>(() => {
    try {
      return sessionStorage.getItem(SEARCH_JOB_KEY);
    } catch {
      return null;
    }
  });
  const [startError, setStartError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  // jobId is only set once the POST resolves, so between the request and
  // the 202 neither jobId nor finished guards a repeat start — two Enter
  // keydowns can even run before the starting state commits. Only a ref
  // read at the top of start closes that window.
  const startingRef = useRef(false);

  const clearJob = useCallback(() => {
    setJobId(null);
    // A stale start/stop error must not survive the job it belonged to.
    setStartError(null);
    try {
      sessionStorage.removeItem(SEARCH_JOB_KEY);
    } catch {
      // best-effort; a stale id only costs one 404 on next mount
    }
    queryClient.removeQueries({ queryKey: ["search-job"] });
  }, [queryClient]);

  const poll = useQuery<SearchJobPage>({
    queryKey: ["search-job", jobId],
    enabled: jobId !== null,
    refetchInterval: (query) => (query.state.data?.finished ? false : POLL_MS),
    queryFn: async ({ signal }): Promise<SearchJobPage> => {
      // A page covers `limit` rows; each poll walks every page so the grid
      // sees the whole job, not just its first 500 rows. The walk honours
      // the query's abort signal, so a superseded or deleted job leaves no
      // in-flight page requests behind.
      const results: SearchResultView[] = [];
      let cursor: string | null = null;
      let last: SearchJobPage | null = null;
      do {
        const page = await api.GET("/search/{id}", {
          params: {
            path: { id: jobId ?? "" },
            query: {
              sort: "-seeders",
              limit: PAGE_SIZE,
              cursor: cursor ?? undefined,
            },
          },
          signal,
        });
        if (!page.data)
          throw new HttpError(
            page.response.status,
            page.error?.detail ?? page.error?.title ?? t("shell.networkError"),
          );
        const data: SearchJobBody = page.data;
        results.push(...(data.results ?? []));
        cursor = data.next_cursor;
        last = {
          finished: data.finished,
          total: data.total,
          engines: data.engines ?? [],
          results,
        };
      } while (cursor !== null);
      if (last === null) throw new HttpError(0, t("shell.networkError"));
      return last;
    },
  });

  // A deleted or purged job answers 404 — the screen returns to its never-run
  // state rather than polling a row that cannot come back.
  useEffect(() => {
    if (poll.error instanceof HttpError && poll.error.status === 404)
      clearJob();
  }, [poll.error, clearJob]);

  const start = useCallback(
    async (q: {
      query: string;
      indexer_ids: string[];
      categories: number[];
    }) => {
      if (startingRef.current) return;
      startingRef.current = true;
      setStarting(true);
      setStartError(null);
      try {
        const { data, error } = await api.POST("/search", {
          body: {
            query: q.query,
            indexer_ids: q.indexer_ids,
            categories: q.categories,
          },
        });
        if (!data) {
          setStartError(
            error?.detail ?? error?.title ?? t("shell.networkError"),
          );
          return;
        }
        setJobId(data.id);
        try {
          sessionStorage.setItem(SEARCH_JOB_KEY, data.id);
        } catch {
          // best-effort; the job still runs without resume
        }
      } finally {
        startingRef.current = false;
        setStarting(false);
      }
    },
    [t],
  );

  const stop = useCallback(async () => {
    if (jobId === null) return;
    const { error, response } = await api.DELETE("/search/{id}", {
      params: { path: { id: jobId } },
    });
    if (error && response.status !== 404) {
      setStartError(error.detail ?? error.title ?? t("shell.networkError"));
      return;
    }
    clearJob();
  }, [jobId, clearJob, t]);

  const data = poll.data;
  return {
    start,
    stop,
    jobId,
    starting,
    finished: data?.finished ?? false,
    total: data?.total ?? 0,
    engines: data?.engines ?? [],
    results: data?.results ?? [],
    error: startError ?? (poll.error ? poll.error.message : null),
  };
}

/** One status-strip chip: queued ○, searching ◐, done ● N, error ✕ with the
 *  error message on hover and focus (doc 09 §7). */
function EngineChip({ engine }: { engine: EngineStatusView }): JSX.Element {
  const { t, i18n } = useTranslation();
  const glyph =
    engine.status === "done"
      ? "●"
      : engine.status === "searching"
        ? "◐"
        : engine.status === "error"
          ? "✕"
          : "○";
  const label =
    engine.status === "done"
      ? `${engine.name} ${formatInteger(engine.count, i18n.language)}`
      : engine.name;

  const chipClass = `inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs ${
    engine.status === "error" ? "text-destructive" : ""
  }`;
  const chip = (
    <>
      <span aria-hidden="true">{glyph}</span>
      {label}
    </>
  );

  if (engine.status !== "error")
    return <span className={chipClass}>{chip}</span>;
  // A button: the error tooltip opens on hover and on click-to-focus alike.
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className={chipClass}>
          {chip}
        </button>
      </TooltipTrigger>
      <TooltipContent>
        {engine.error ?? t("search.engineFailed")}
      </TooltipContent>
    </Tooltip>
  );
}

export function SearchScreen(): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const search = useSearchJob();

  const indexersQuery = useQuery({
    queryKey: ["indexers"],
    queryFn: async () => {
      const { data, error } = await api.GET("/indexers");
      if (!data) throw new Error(error?.detail ?? t("shell.networkError"));
      return data.indexers ?? [];
    },
  });
  const categoriesQuery = useQuery({
    queryKey: ["indexer-categories"],
    queryFn: async () => {
      const { data, error } = await api.GET("/indexers/categories");
      if (!data) throw new Error(error?.detail ?? t("shell.networkError"));
      return data.categories ?? [];
    },
  });

  const [queryText, setQueryText] = useState("");
  const searchPrefs = useUiPrefs((s) => s.search);
  // null = "every enabled indexer" (untouched); a Set pins the user's picks
  // (doc 09 §7). The document's empty array is the untouched state, so an
  // explicit empty pick exists only in local state — a reload reads [] as
  // untouched again.
  const [picked, setPicked] = useState<Set<string> | null>(() =>
    searchPrefs.indexerIds.length === 0
      ? null
      : new Set(searchPrefs.indexerIds),
  );
  // The picker is single-select; the document member is a list so a saved
  // search can carry several.
  const [category, setCategory] = useState<number | null>(
    () => searchPrefs.categories[0] ?? null,
  );
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [resolved, setResolved] = useState<Map<string, string | null>>(
    new Map(),
  );
  const [flashing, setFlashing] = useState<Set<string>>(new Set());
  const [failed, setFailed] = useState<Set<string>>(new Set());
  const [dialogIds, setDialogIds] = useState<string[] | null>(null);
  // The saved search whose run is on the wire, with the total it had stored
  // when the run started — the finish effect diffs the new total against it.
  const [savedRun, setSavedRun] = useState<{
    id: string;
    previousTotal: number;
  } | null>(null);
  // Per-entry count of results a later run added over lastTotal — the "new
  // since last view" badge on the Saved menu.
  const [newSince, setNewSince] = useState<Map<string, number>>(new Map());

  const indexers = useMemo(
    () => indexersQuery.data ?? [],
    [indexersQuery.data],
  );
  const enabled = useMemo(
    () => indexers.filter((ix) => ix.enabled),
    [indexers],
  );
  const enabledIds = useMemo(
    () => new Set(enabled.map((ix) => ix.id)),
    [enabled],
  );
  const allIds = useMemo(
    () => new Set(indexers.map((ix) => ix.id)),
    [indexers],
  );

  // Selection writes go through the prefs document (doc 09 §7): the store
  // marks the member dirty and the debounced writer PUTs it whole.
  const writeIndexerIds = useCallback((next: Set<string> | null) => {
    setPicked(next);
    const current = useUiPrefs.getState().search;
    useUiPrefs.getState().patch({
      search: { ...current, indexerIds: next === null ? [] : [...next] },
    });
  }, []);

  const writeCategory = useCallback((next: number | null) => {
    setCategory(next);
    const current = useUiPrefs.getState().search;
    useUiPrefs.getState().patch({
      search: { ...current, categories: next === null ? [] : [next] },
    });
  }, []);

  // The document's selection lands here when hydrate or another writer
  // changes it. An echo of this screen's own write is skipped, so an
  // explicit empty pick is not flattened back into "all".
  useEffect(() => {
    const stored = searchPrefs.indexerIds;
    setPicked((prev) => {
      const echo =
        prev !== null
          ? stored.length === prev.size && stored.every((id) => prev.has(id))
          : stored.length === 0;
      if (echo) return prev;
      return stored.length === 0 ? null : new Set(stored);
    });
  }, [searchPrefs.indexerIds]);

  useEffect(() => {
    const stored = searchPrefs.categories[0] ?? null;
    setCategory((prev) => (prev === stored ? prev : stored));
  }, [searchPrefs.categories]);

  // One-time migration: the selection lives in the prefs document now, so
  // the sessionStorage copy T063 wrote must not outlive it.
  useEffect(() => {
    try {
      sessionStorage.removeItem(SEARCH_STATE_KEY);
    } catch {
      // sessionStorage can be unavailable; a stale key is harmless.
    }
  }, []);

  // A stored pick of an indexer that no longer exists is pruned once the
  // list lands (doc 09 §7: the selection is sent only after the
  // server-derived effective list).
  useEffect(() => {
    if (!indexersQuery.data) return;
    const pruned = searchPrefs.indexerIds.filter((id) => allIds.has(id));
    if (pruned.length !== searchPrefs.indexerIds.length)
      writeIndexerIds(new Set(pruned));
  }, [indexersQuery.data, allIds, searchPrefs.indexerIds, writeIndexerIds]);

  const effectiveIndexerIds = useMemo(() => {
    if (picked === null) return enabled.map((ix) => ix.id);
    return [...picked].filter((id) => enabledIds.has(id));
  }, [picked, enabled, enabledIds]);

  const seedersUnknown = useMemo(
    () =>
      new Set(indexers.filter((ix) => ix.seeders_unknown).map((ix) => ix.id)),
    [indexers],
  );

  const categories = useMemo(
    () => categoriesQuery.data ?? [],
    [categoriesQuery.data],
  );

  const byId = useMemo(
    () => new Map(search.results.map((r) => [r.id, r])),
    [search.results],
  );

  const rowState = useMemo<ResultRowState>(
    () => ({ resolved, failed, seedersUnknown, flashing }),
    [resolved, failed, seedersUnknown, flashing],
  );

  const markResolved = useCallback(
    (ids: string[], taskIds: (string | null)[]) => {
      setResolved((prev) => {
        const next = new Map(prev);
        ids.forEach((id, i) => next.set(id, taskIds[i] ?? null));
        return next;
      });
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of ids) next.delete(id);
        return next;
      });
      setFailed((prev) => {
        const next = new Set(prev);
        for (const id of ids) next.delete(id);
        return next;
      });
      // Flash merges into any set already running — a second chunk landing
      // inside FLASH_MS must not clobber the first, and each timer drops
      // only its own ids.
      setFlashing((prev) => new Set([...prev, ...ids]));
      window.setTimeout(() => {
        setFlashing((prev) => {
          const next = new Set(prev);
          for (const id of ids) next.delete(id);
          return next;
        });
      }, FLASH_MS);
    },
    [],
  );

  /** Marks ids whose last submit failed. The rows stay selectable: a
   *  transport error's outcome is unknown and a 5xx or 429 may succeed on
   *  retry (a resubmit of a committed id answers /problems/conflict, which
   *  is terminal success). */
  const markFailed = useCallback((ids: string[]) => {
    if (ids.length === 0) return;
    setFailed((prev) => {
      const next = new Set(prev);
      for (const id of ids) next.add(id);
      return next;
    });
  }, []);

  /** One chunk's 201 outcome: created ids and /problems/conflict rejections
   *  resolve to ✓ (a conflict is terminal success — the task already
   *  exists); every other rejected id marks its row failed. Returns the
   *  non-conflict rejections for the caller's summary toast. */
  const applyChunkOutcome = useCallback(
    (
      chunk: string[],
      created: NonNullable<CreateOutput["created"]>,
      rejected: RejectedURI[],
    ): RejectedURI[] => {
      const rejectedIds = new Set(
        rejected.map((r) => r.search_result_id).filter(Boolean) as string[],
      );
      const conflicts = new Set(
        rejected
          .filter((r) => r.type === CONFLICT_TYPE)
          .map((r) => r.search_result_id)
          .filter(Boolean) as string[],
      );
      // Created tasks key back to their res_ id through the rendered
      // display reference — never by position, so a reordered created[]
      // cannot label a row with another row's task.
      const taskByRes = new Map<string, string>();
      for (const task of created) {
        const src = task.source_uri;
        if (typeof src === "string" && src.startsWith(RESULT_DISPLAY_PREFIX))
          taskByRes.set(src.slice(RESULT_DISPLAY_PREFIX.length), task.id);
      }
      const resolvedIds = chunk.filter((id) => !rejectedIds.has(id));
      markResolved(
        [...resolvedIds, ...conflicts],
        [
          ...resolvedIds.map((id) => taskByRes.get(id) ?? null),
          ...[...conflicts].map(() => null),
        ],
      );
      const failures = rejected.filter(
        (r) => !conflicts.has(r.search_result_id ?? ""),
      );
      markFailed(
        failures.map((r) => r.search_result_id).filter(Boolean) as string[],
      );
      return failures;
    },
    [markFailed, markResolved],
  );

  /** One POST /tasks per chunk of ≤50 ids (the body's maxItems cap). The
   *  opaque res_ ids are the only payload — provider URLs and magnets never
   *  reach the browser. */
  const submitIds = useCallback(
    async (ids: string[], singleTitle?: string) => {
      const destination = useUiPrefs.getState().lastDestination ?? undefined;
      for (let i = 0; i < ids.length; i += MAX_SUBMIT_IDS) {
        const chunk = ids.slice(i, i + MAX_SUBMIT_IDS);
        let data: CreateOutput | undefined;
        let detail = t("shell.networkError");
        try {
          const out = await api.POST("/tasks", {
            body: { search_result_ids: chunk, destination },
          });
          if (out.response.status === 201 && out.data) data = out.data;
          else
            detail =
              out.error?.detail ?? out.error?.title ?? t("shell.networkError");
        } catch {
          // transport error — the chunk's rows stay selectable for retry
        }
        if (data) {
          const failures = applyChunkOutcome(
            chunk,
            data.created ?? [],
            data.rejected ?? [],
          );
          if (failures.length > 0) {
            const first = byId.get(failures[0].search_result_id ?? "");
            toast.error(
              singleTitle !== undefined && failures.length === 1
                ? t("search.downloadFailed", {
                    title: singleTitle,
                    detail: failures[0].detail ?? failures[0].type,
                  })
                : t("search.bulkRejected", {
                    count: failures.length,
                    title: first?.title ?? failures[0].search_result_id,
                  }),
            );
          } else if (singleTitle !== undefined) {
            // One-click add resolved: the toast names the created task, the
            // row's ✓ links to it.
            const createdName = (data.created ?? []).find(
              (task) =>
                task.source_uri === `${RESULT_DISPLAY_PREFIX}${chunk[0]}`,
            )?.name;
            toast.success(
              t("search.addedToQueue", {
                title: createdName ?? singleTitle,
              }),
            );
          }
        } else {
          // Transport error or non-201: every id in the chunk is failed.
          markFailed(chunk);
          toast.error(
            singleTitle !== undefined
              ? t("search.downloadFailed", { title: singleTitle, detail })
              : t("search.chunkFailed", { count: chunk.length, detail }),
          );
        }
      }
    },
    [applyChunkOutcome, byId, markFailed, markResolved, t],
  );

  /** The "Download to…" dialog reports each chunk's outcome here so its rows
   *  reach the same terminal states the immediate path produces. */
  const onSearchResultOutcome = useCallback<SearchResultOutcome>(
    (ids, created, rejected, detail) => {
      if (rejected === null) {
        markFailed(ids);
        toast.error(
          t("search.chunkFailed", {
            count: ids.length,
            detail: detail ?? t("shell.networkError"),
          }),
        );
        return;
      }
      const failures = applyChunkOutcome(ids, created, rejected);
      if (failures.length > 0) {
        const first = byId.get(failures[0].search_result_id ?? "");
        toast.error(
          t("search.bulkRejected", {
            count: failures.length,
            title: first?.title ?? failures[0].search_result_id,
          }),
        );
      }
    },
    [applyChunkOutcome, byId, markFailed, t],
  );

  const runSearch = useCallback(() => {
    const query = queryText.trim();
    // The submit button's disabled state guards the click path; the guard
    // lives here too so the input's Enter key cannot start a second job
    // while one runs — an orphaned job is never DELETE'd.
    if (
      query === "" ||
      search.starting ||
      (search.jobId !== null && !search.finished)
    )
      return;
    setSelected(new Set());
    setResolved(new Map());
    setFailed(new Set());
    void search.start({
      query,
      indexer_ids: effectiveIndexerIds,
      categories: category === null ? [] : [category],
    });
  }, [queryText, effectiveIndexerIds, category, search]);

  /** Re-runs a saved search: the screen picks up the stored query and both
   *  selections, then the job starts with exactly the stored indexerIds and
   *  categories — never the effective defaults (task T064 step 5). */
  const runSavedSearch = useCallback(
    (s: SavedSearch) => {
      if (search.starting || (search.jobId !== null && !search.finished))
        return;
      setQueryText(s.query);
      const current = useUiPrefs.getState().search;
      useUiPrefs.getState().patch({
        search: {
          ...current,
          indexerIds: s.indexerIds,
          categories: s.categories,
        },
      });
      setSelected(new Set());
      setResolved(new Map());
      setFailed(new Set());
      setSavedRun({ id: s.id, previousTotal: s.lastTotal });
      setNewSince((prev) => {
        const next = new Map(prev);
        next.delete(s.id);
        return next;
      });
      void search.start({
        query: s.query,
        indexer_ids: s.indexerIds,
        categories: s.categories,
      });
    },
    [search],
  );

  // A saved search's finished run writes lastTotal back to its entry; a
  // higher total than the stored one lands in the entry's "new since last
  // view" badge.
  useEffect(() => {
    if (savedRun === null || !search.finished) return;
    const current = useUiPrefs.getState().search;
    const entry = current.saved.find((e) => e.id === savedRun.id);
    if (entry !== undefined && entry.lastTotal !== search.total)
      useUiPrefs.getState().patch({
        search: {
          ...current,
          saved: current.saved.map((e) =>
            e.id === savedRun.id ? { ...e, lastTotal: search.total } : e,
          ),
        },
      });
    const delta = search.total - savedRun.previousTotal;
    if (delta > 0) setNewSince((prev) => new Map(prev).set(savedRun.id, delta));
    setSavedRun(null);
  }, [savedRun, search.finished, search.total]);

  const indexersLoaded = indexersQuery.isSuccess;
  const noEnabled = indexersLoaded && enabled.length === 0;
  const allFailed =
    search.jobId !== null &&
    search.finished &&
    search.engines.length > 0 &&
    search.engines.every((e) => e.status === "error");
  const ranEmpty =
    search.jobId !== null &&
    search.finished &&
    !allFailed &&
    search.results.length === 0;
  const canSearch = queryText.trim() !== "" && effectiveIndexerIds.length > 0;
  const selectedSize = useMemo(() => {
    let sum = 0;
    for (const id of selected) sum += byId.get(id)?.size_bytes ?? 0;
    return sum;
  }, [selected, byId]);

  const toggleIndexer = (id: string, on: boolean) => {
    const base =
      picked === null ? new Set(enabled.map((ix) => ix.id)) : new Set(picked);
    if (on) base.add(id);
    else base.delete(id);
    writeIndexerIds(base);
  };

  return (
    <TooltipProvider>
      <div className="flex h-full min-h-0 flex-col gap-2 p-3">
        <h1 className="sr-only">{t("screens.search")}</h1>
        <div className="flex flex-wrap items-end gap-2">
          <div className="min-w-48 flex-1">
            <Label htmlFor="search-query" className="sr-only">
              {t("search.queryLabel")}
            </Label>
            <Input
              id="search-query"
              value={queryText}
              placeholder={t("search.queryPlaceholder")}
              onChange={(e) => setQueryText(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && canSearch) runSearch();
              }}
            />
          </div>

          <Popover>
            <PopoverTrigger asChild>
              <Button variant="outline" size="sm">
                {t("search.indexerCount", {
                  selected: effectiveIndexerIds.length,
                  total: enabled.length,
                })}{" "}
                <ChevronDown aria-hidden="true" />
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-72 gap-1 p-1.5">
              <div className="flex gap-1 px-1 pb-1">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => writeIndexerIds(null)}
                >
                  {t("search.all")}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => writeIndexerIds(new Set())}
                >
                  {t("search.none")}
                </Button>
              </div>
              <ul className="max-h-64 overflow-auto">
                {indexers.map((ix) => {
                  const checked =
                    picked === null ? ix.enabled : picked.has(ix.id);
                  return (
                    <li
                      key={ix.id}
                      className={`flex items-center gap-2 rounded px-1 py-0.5 ${
                        ix.enabled ? "" : "text-muted-foreground"
                      }`}
                    >
                      <Checkbox
                        aria-label={ix.name}
                        checked={checked}
                        disabled={!ix.enabled}
                        onCheckedChange={(c) =>
                          toggleIndexer(ix.id, c === true)
                        }
                      />
                      <span
                        aria-hidden="true"
                        className={`inline-block size-2 rounded-full ${
                          ix.last_error === null
                            ? "bg-[var(--ok)]"
                            : "bg-destructive"
                        }`}
                      />
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="min-w-0 flex-1 truncate">
                            {ix.name}
                          </span>
                        </TooltipTrigger>
                        {ix.last_error !== null && (
                          <TooltipContent>{ix.last_error}</TooltipContent>
                        )}
                      </Tooltip>
                      {!ix.enabled && (
                        <Link
                          to="/settings/indexers"
                          aria-label={t("search.configureIndexer", {
                            name: ix.name,
                          })}
                          className="text-xs underline"
                        >
                          {t("search.configure")}
                        </Link>
                      )}
                    </li>
                  );
                })}
              </ul>
            </PopoverContent>
          </Popover>

          <Select
            value={category === null ? "__all__" : String(category)}
            onValueChange={(v) =>
              writeCategory(v === "__all__" ? null : Number(v))
            }
          >
            <SelectTrigger
              size="sm"
              aria-label={t("search.category")}
              className="w-44"
            >
              <SelectValue placeholder={t("search.category")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__all__">{t("search.categoryAll")}</SelectItem>
              {categories.map((c) => (
                <SelectItem key={c.id} value={String(c.id)}>
                  {c.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Button
            size="sm"
            disabled={
              !canSearch ||
              search.starting ||
              (search.jobId !== null && !search.finished)
            }
            onClick={runSearch}
          >
            {t("search.submit")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            aria-label={t("search.stopAriaLabel")}
            disabled={search.jobId === null || search.finished}
            onClick={() => void search.stop()}
          >
            {t("search.stop")}
          </Button>
          <SaveSearchButton
            query={queryText}
            indexerIds={effectiveIndexerIds}
            categories={category === null ? [] : [category]}
          />
          <SavedSearchesMenu onRun={runSavedSearch} newSince={newSince} />
        </div>

        {search.jobId !== null && (
          <div
            aria-live="polite"
            aria-label={t("search.statusStrip")}
            className="flex flex-wrap gap-1.5"
          >
            {search.engines.map((e) => (
              <EngineChip key={e.id} engine={e} />
            ))}
          </div>
        )}

        {search.error !== null && (
          <p role="alert" className="text-sm text-destructive">
            {search.error}
          </p>
        )}

        {noEnabled ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 text-sm text-muted-foreground">
            <p>{t("search.noIndexers")}</p>
            <Link to="/settings/indexers" className="underline">
              {t("search.noIndexersCta")}
            </Link>
          </div>
        ) : allFailed ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 text-sm">
            <p className="font-medium">{t("search.allFailed")}</p>
            <ul className="text-muted-foreground">
              {search.engines.map((e) => (
                <li key={e.id}>
                  {e.name}: {e.error ?? t("search.engineFailed")}
                </li>
              ))}
            </ul>
            <Button size="sm" variant="outline" onClick={runSearch}>
              {t("actions.retry")}
            </Button>
          </div>
        ) : ranEmpty ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-1 text-sm text-muted-foreground">
            <p>
              {t("empty.searchResults", {
                query: queryText,
                count: search.engines.length,
              })}
            </p>
            <p>{t("actions.tryFewerFilters")}</p>
          </div>
        ) : search.jobId === null ? (
          <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
            <p>{t("empty.search")}</p>
          </div>
        ) : (
          <>
            <ResultRowStateContext.Provider value={rowState}>
              <ResultsGrid
                results={search.results}
                total={search.total}
                selected={selected}
                onSelectedChange={setSelected}
                onDownload={(r) => void submitIds([r.id], r.title)}
              />
            </ResultRowStateContext.Provider>
            <div className="flex items-center gap-3 border-t pt-2 text-sm">
              <span className="tabular-nums text-muted-foreground">
                {selected.size > 0 &&
                  `${t("search.selectedCount", { count: selected.size })} · ${formatBytes(selectedSize, locale)}`}
              </span>
              <DropdownMenu.Root>
                <DropdownMenu.Trigger asChild>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={selected.size === 0}
                  >
                    {t("search.downloadSelected")}{" "}
                    <ChevronDown aria-hidden="true" />
                  </Button>
                </DropdownMenu.Trigger>
                <DropdownMenu.Portal>
                  <DropdownMenu.Content
                    className={menuContentClass}
                    align="start"
                  >
                    <DropdownMenu.Item
                      className={menuItemClass}
                      onSelect={() => void submitIds([...selected])}
                    >
                      {t("search.downloadNow")}
                    </DropdownMenu.Item>
                    <DropdownMenu.Item
                      className={menuItemClass}
                      onSelect={() => setDialogIds([...selected])}
                    >
                      {t("search.downloadTo")}
                    </DropdownMenu.Item>
                  </DropdownMenu.Content>
                </DropdownMenu.Portal>
              </DropdownMenu.Root>
            </div>
          </>
        )}

        <AddTaskDialog
          open={dialogIds !== null}
          onOpenChange={(open) => {
            if (!open) setDialogIds(null);
          }}
          initialSearchResultIds={dialogIds ?? undefined}
          onSearchResultOutcome={onSearchResultOutcome}
        />
      </div>
    </TooltipProvider>
  );
}
