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
import { toast } from "sonner";
import { CircleHelp, Copy, Plus, Trash2 } from "lucide-react";

import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { cn } from "../../lib/utils";
import { formatWhen } from "../../lib/format";
import strings from "../../locales/en/rss.json";
import { useFeeds } from "./FeedsScreen";
import { FolderBrowserDialog } from "../FolderBrowser/FolderBrowserDialog";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
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

initI18n().addResourceBundle("en", "rss", strings);

// The rule document is the generated schema type; the editor never defines
// its own copy.
export type RuleDoc = components["schemas"]["RuleDoc"];
export type DryRunItem = components["schemas"]["DryRunItem"];
type RuleDTO = components["schemas"]["RuleDTO"];
type CategoryDTO = components["schemas"]["CategoryDTO"];
type ErrorDetail = components["schemas"]["ErrorDetail"];

export interface EditorState {
  doc: RuleDoc;
  dirty: boolean;
  preview: {
    evaluated: number;
    matched: number;
    elapsed_ms: number;
    results: DryRunItem[];
  } | null;
  fieldErrors: Record<string, string>; // location -> message, from the 422 errors[]
}

export const PREVIEW_DEBOUNCE_MS = 250;
export const PREVIEW_LIMIT = 50; // "matches N of the last 50 items"
const CONFLICT = 409;
const UNPROCESSABLE = 422;
const CATEGORY_NONE = "__none__"; // Radix Select rejects an empty item value
const rulesKey = ["rss-rules"] as const;
const categoriesKey = ["categories"] as const;

// The two prefixes doc 05 section 10.3 pins: the dry run reports
// body.rule.*, save reports body.definition.* — both map onto one control.
const LOCATION_PREFIXES = ["body.rule.", "body.definition."];

/** Strip the pinned prefix and any [i] index suffix, so body.rule.match.
 *  any_of[0] and body.definition.match.any_of land on the same key. */
export function fieldKey(location: string): string {
  let path = location;
  for (const prefix of LOCATION_PREFIXES) {
    if (path.startsWith(prefix)) {
      path = path.slice(prefix.length);
      break;
    }
  }
  return path.replace(/\[\d+\]/g, "");
}

function mapFieldErrors(errors: ErrorDetail[]): Record<string, string> {
  const mapped: Record<string, string> = {};
  for (const entry of errors ?? []) {
    if (entry.location === undefined || entry.message === undefined) continue;
    mapped[fieldKey(entry.location)] = entry.message;
  }
  return mapped;
}

/** Convert the response's [start,end) UTF-8 byte offsets into JavaScript
 *  UTF-16 code-unit indices so the title can be sliced. The two index
 *  systems coincide only for ASCII titles (doc 05 section 10.3). */
export function utf8SpanToUtf16(
  title: string,
  span: number[],
): [number, number] | null {
  const [byteStart, byteEnd] = span;
  if (byteEnd <= byteStart || byteStart < 0) return null;
  const encoder = new TextEncoder();
  let bytes = 0;
  let units = 0;
  let start = -1;
  // for..of iterates code points; char.length is 2 for an astral pair.
  for (const char of title) {
    if (start < 0 && bytes >= byteStart) start = units;
    if (bytes >= byteEnd) return start < 0 ? null : [start, units];
    bytes += encoder.encode(char).length;
    units += char.length;
  }
  if (start < 0 && bytes >= byteStart) start = units;
  return start < 0 || bytes < byteEnd ? null : [start, units];
}

/** One pattern per line; empty lines are dropped (an empty entry is a save
 *  error, and doc 08 section 4.3 forbids ever splitting on "|"). */
function linesToPatterns(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

function blankDoc(name: string): RuleDoc {
  return {
    name,
    enabled: true,
    match: { mode: "wildcard", fields: ["title"] },
    action: {},
  };
}

/** The debounced dry run of steps 3-6: 250 ms after the last change it
 *  posts the in-progress document, aborting any in-flight request. A 422
 *  leaves the previous preview on screen and surfaces the field errors;
 *  a success clears them. */
export function useRulePreview(
  doc: RuleDoc | null,
  feedIds: string[],
  ignoreState: boolean,
): {
  preview: EditorState["preview"];
  fieldErrors: EditorState["fieldErrors"];
  pending: boolean;
} {
  const [preview, setPreview] = useState<EditorState["preview"]>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [pending, setPending] = useState(false);

  useEffect(() => {
    if (doc === null) return;
    const controller = new AbortController();
    const timer = setTimeout(() => {
      setPending(true);
      void (async () => {
        try {
          const { data, error, response } = await api.POST("/rules/test", {
            body: {
              rule: doc,
              feeds: feedIds.length > 0 ? feedIds : undefined,
              limit: PREVIEW_LIMIT,
              ignore_state: ignoreState,
            },
            signal: controller.signal,
          });
          if (controller.signal.aborted) return;
          if (data) {
            setPreview({
              evaluated: data.evaluated,
              matched: data.matched,
              elapsed_ms: data.elapsed_ms,
              results: data.results ?? [],
            });
            setFieldErrors({});
          } else if (
            response.status === UNPROCESSABLE &&
            error?.errors?.length
          ) {
            // The panel freezes on the last valid result (doc 09 section 8.2).
            setFieldErrors(mapFieldErrors(error.errors));
          }
        } catch {
          // Aborted or unreachable: keep the last preview either way.
        } finally {
          if (!controller.signal.aborted) setPending(false);
        }
      })();
    }, PREVIEW_DEBOUNCE_MS);

    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [doc, feedIds, ignoreState]);

  return { preview, fieldErrors, pending };
}

/** A title with its matched span marked; highlight arrives as UTF-8 byte
 *  offsets, so it is converted before slicing. */
function HighlightedTitle({ row }: { row: DryRunItem }): JSX.Element {
  const span =
    row.highlight !== undefined
      ? utf8SpanToUtf16(row.title, row.highlight)
      : null;
  if (span === null) return <>{row.title}</>;
  const [start, end] = span;
  return (
    <>
      {row.title.slice(0, start)}
      <mark className="bg-accent/50">{row.title.slice(start, end)}</mark>
      {row.title.slice(end)}
    </>
  );
}

export function RuleEditor(): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const queryClient = useQueryClient();
  const { feeds } = useFeeds();

  const rulesQuery = useQuery({
    queryKey: rulesKey,
    queryFn: async (): Promise<RuleDTO[]> => {
      const { data, error } = await api.GET("/rules");
      if (!data)
        throw new Error(
          error?.detail ?? error?.title ?? t("shell.networkError"),
        );
      return data.rules ?? [];
    },
  });
  const categoriesQuery = useQuery({
    queryKey: categoriesKey,
    queryFn: async (): Promise<CategoryDTO[]> => {
      const { data, error } = await api.GET("/categories");
      if (!data)
        throw new Error(
          error?.detail ?? error?.title ?? t("shell.networkError"),
        );
      return data.categories ?? [];
    },
  });
  const rules = useMemo(() => rulesQuery.data ?? [], [rulesQuery.data]);
  const categories = useMemo(
    () => categoriesQuery.data ?? [],
    [categoriesQuery.data],
  );

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [doc, setDoc] = useState<RuleDoc>(() =>
    blankDoc(t("rss:rules.newRule")),
  );
  const [dirty, setDirty] = useState(false);
  const [ignoreState, setIgnoreState] = useState(true);
  const [saveErrors, setSaveErrors] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  const [tagDraft, setTagDraft] = useState("");
  const [browseOpen, setBrowseOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<RuleDTO | null>(null);
  const [runConfirm, setRunConfirm] = useState(false);
  const [testTitle, setTestTitle] = useState("");
  const [testResult, setTestResult] = useState<DryRunItem | null>(null);
  const [testError, setTestError] = useState<string | null>(null);
  const [testBusy, setTestBusy] = useState(false);
  const importInput = useRef<HTMLInputElement>(null);

  const selected = rules.find((rule) => rule.id === selectedId) ?? null;

  // The preview's feeds member is ids; the document scopes by URL, so an
  // empty selection sends nothing and the server resolves rule.feeds.
  const previewFeedIds = useMemo(() => {
    if (!doc.feeds || doc.feeds.length === 0) return [];
    const wanted = new Set(doc.feeds);
    return feeds.filter((feed) => wanted.has(feed.url)).map((feed) => feed.id);
  }, [doc.feeds, feeds]);

  const {
    preview,
    fieldErrors: previewErrors,
    pending,
  } = useRulePreview(doc, previewFeedIds, ignoreState);
  // Save-time 422s use the body.definition. prefix; fieldKey maps them onto
  // the same controls, so the two maps merge into one lookup.
  const fieldErrors = useMemo(
    () => ({ ...previewErrors, ...saveErrors }),
    [previewErrors, saveErrors],
  );

  const update = useCallback((mutate: (draft: RuleDoc) => void) => {
    setDoc((prev) => {
      const next = structuredClone(prev);
      mutate(next);
      return next;
    });
    setDirty(true);
  }, []);

  const invalidateRules = useCallback(
    () => queryClient.invalidateQueries({ queryKey: rulesKey }),
    [queryClient],
  );

  const fail = useCallback(
    (detail: string | undefined) =>
      toast.error(
        t("rss:toast.requestFailed", {
          detail: detail ?? t("shell.networkError"),
        }),
      ),
    [t],
  );

  const selectRule = useCallback((rule: RuleDTO) => {
    setSelectedId(rule.id);
    setDoc(structuredClone(rule.definition));
    setDirty(false);
    setSaveErrors({});
    setTestResult(null);
    setTestError(null);
  }, []);

  const newRule = useCallback(() => {
    setSelectedId(null);
    setDoc(blankDoc(t("rss:rules.newRule")));
    setDirty(false);
    setSaveErrors({});
    setTestResult(null);
    setTestError(null);
  }, [t]);

  const duplicateRule = useCallback(() => {
    if (selected === null) return;
    const copy = structuredClone(selected.definition);
    copy.name = t("rss:rules.duplicateName", { name: selected.name });
    setSelectedId(null);
    setDoc(copy);
    setDirty(true);
    setSaveErrors({});
  }, [selected, t]);

  const save = useCallback(async () => {
    if (saving || doc.name.trim() === "") return;
    setSaving(true);
    setSaveErrors({});
    try {
      const out =
        selectedId === null
          ? await api.POST("/rules", {
              body: { name: doc.name, definition: doc },
            })
          : await api.PATCH("/rules/{id}", {
              params: { path: { id: selectedId } },
              body: { name: doc.name, enabled: doc.enabled, definition: doc },
            });
      const { data, error, response } = out;
      if (data) {
        setSelectedId(data.id);
        setDoc(structuredClone(data.definition));
        setDirty(false);
        await invalidateRules();
        return;
      }
      if (response.status === UNPROCESSABLE && error?.errors?.length) {
        setSaveErrors(mapFieldErrors(error.errors));
      } else if (response.status === CONFLICT) {
        toast.error(t("rss:rules.saveConflict", { name: doc.name }));
      } else {
        fail(error?.detail ?? error?.title);
      }
    } catch {
      fail(undefined);
    } finally {
      setSaving(false);
    }
  }, [saving, doc, selectedId, invalidateRules, fail, t]);

  const removeRule = useCallback(
    async (rule: RuleDTO) => {
      try {
        const { error } = await api.DELETE("/rules/{id}", {
          params: { path: { id: rule.id } },
        });
        if (error) {
          fail(error.detail ?? error.title);
          return;
        }
      } catch {
        fail(undefined);
        return;
      }
      setDeleteTarget(null);
      if (selectedId === rule.id) newRule();
      await invalidateRules();
    },
    [fail, invalidateRules, newRule, selectedId],
  );

  const toggleEnabled = useCallback(
    async (rule: RuleDTO, enabled: boolean) => {
      try {
        const { error } = await api.PATCH("/rules/{id}", {
          params: { path: { id: rule.id } },
          body: { enabled },
        });
        if (error) {
          fail(error.detail ?? error.title);
          return;
        }
      } catch {
        fail(undefined);
        return;
      }
      if (rule.id === selectedId) update((draft) => (draft.enabled = enabled));
      await invalidateRules();
    },
    [fail, invalidateRules, selectedId, update],
  );

  const runExisting = useCallback(async () => {
    if (selected === null) return;
    setRunConfirm(false);
    try {
      const { data, error } = await api.POST("/rules/{id}/run", {
        params: { path: { id: selected.id } },
      });
      if (!data) {
        fail(error?.detail ?? error?.title);
        return;
      }
      toast.success(
        t("rss:rules.runResult", {
          evaluated: data.evaluated,
          matched: data.matched,
          created: data.created_task_ids?.length ?? 0,
        }),
      );
      await invalidateRules();
    } catch {
      fail(undefined);
    }
  }, [selected, fail, invalidateRules, t]);

  const runTitleTest = useCallback(async () => {
    const title = testTitle.trim();
    if (title === "" || testBusy) return;
    setTestBusy(true);
    setTestError(null);
    try {
      const { data, error } = await api.POST("/rules/test", {
        body: {
          rule: doc,
          titles: [title],
          limit: PREVIEW_LIMIT,
          ignore_state: true,
        },
      });
      if (data) {
        setTestResult(data.results?.[0] ?? null);
      } else {
        setTestError(error?.detail ?? error?.title ?? t("shell.networkError"));
        setTestResult(null);
      }
    } catch {
      setTestError(t("shell.networkError"));
      setTestResult(null);
    } finally {
      setTestBusy(false);
    }
  }, [doc, testTitle, testBusy, t]);

  const exportRules = useCallback(() => {
    const payload = rules.map((rule) => ({
      name: rule.name,
      definition: rule.definition,
    }));
    const blob = new Blob([JSON.stringify(payload, null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "dl-tool-rules.json";
    anchor.click();
    URL.revokeObjectURL(url);
  }, [rules]);

  const importRules = useCallback(
    async (file: File) => {
      let parsed: unknown;
      try {
        parsed = JSON.parse(await file.text());
      } catch {
        toast.error(t("rss:rules.importInvalid"));
        return;
      }
      const entries = Array.isArray(parsed) ? parsed : [parsed];
      const skipped: string[] = [];
      const failed: string[] = [];
      let imported = 0;
      for (const entry of entries) {
        const candidate = entry as { name?: unknown; definition?: unknown };
        if (
          typeof candidate?.name !== "string" ||
          typeof candidate?.definition !== "object" ||
          candidate.definition === null
        ) {
          toast.error(t("rss:rules.importInvalid"));
          return;
        }
        const name = candidate.name;
        try {
          const { error, response } = await api.POST("/rules", {
            body: {
              name,
              definition: candidate.definition as RuleDoc,
            },
          });
          if (error === undefined) {
            imported++;
          } else if (response.status === CONFLICT) {
            skipped.push(name);
          } else {
            failed.push(name);
          }
        } catch {
          failed.push(name);
        }
      }
      if (imported > 0)
        toast.success(t("rss:rules.imported", { count: imported }));
      if (skipped.length > 0)
        toast.info(
          t("rss:rules.importSkipped", {
            count: skipped.length,
            names: skipped.join(", "),
          }),
        );
      if (failed.length > 0)
        toast.error(t("rss:rules.importFailed", { names: failed.join(", ") }));
      await invalidateRules();
    },
    [invalidateRules, t],
  );

  const addTag = useCallback(() => {
    const value = tagDraft.trim();
    if (value === "") return;
    update((draft) => {
      const tags = draft.action.tags ?? [];
      if (!tags.includes(value)) draft.action.tags = [...tags, value];
    });
    setTagDraft("");
  }, [tagDraft, update]);

  const removeTag = useCallback(
    (tag: string) =>
      update((draft) => {
        draft.action.tags = (draft.action.tags ?? []).filter(
          (entry) => entry !== tag,
        );
      }),
    [update],
  );

  const anyOfText = (doc.match.any_of ?? []).join("\n");
  const noneOfText = (doc.match.none_of ?? []).join("\n");
  const shown = preview?.results.slice(0, PREVIEW_LIMIT) ?? [];
  const shownMatched = shown.filter((row) => row.matched).length;

  const fieldAlert = (key: string) =>
    fieldErrors[key] !== undefined ? (
      <p role="alert" className="text-sm text-destructive">
        {fieldErrors[key]}
      </p>
    ) : null;

  return (
    <div className="flex h-full min-h-0" data-testid="rule-editor">
      <div className="flex w-56 shrink-0 flex-col border-r">
        <div className="flex items-center justify-between gap-1 p-2">
          <h2 className="text-sm font-semibold">{t("rss:rules.listLabel")}</h2>
          <span className="flex gap-1">
            <Button
              size="sm"
              variant="outline"
              aria-label={t("rss:rules.add")}
              onClick={newRule}
            >
              <Plus className="size-4" />
            </Button>
            <Button
              size="sm"
              variant="outline"
              aria-label={t("rss:rules.duplicate")}
              disabled={selected === null}
              onClick={duplicateRule}
            >
              <Copy className="size-4" />
            </Button>
            <Button
              size="sm"
              variant="outline"
              aria-label={t("rss:rules.delete")}
              disabled={selected === null}
              onClick={() => setDeleteTarget(selected)}
            >
              <Trash2 className="size-4" />
            </Button>
          </span>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-1">
          {rulesQuery.isError ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 p-4 text-center">
              <p role="alert" className="text-sm text-destructive">
                {t("rss:rules.loadError")}
              </p>
              <Button
                size="sm"
                variant="outline"
                onClick={() => void rulesQuery.refetch()}
              >
                {t("rss:rules.retry")}
              </Button>
            </div>
          ) : rules.length === 0 ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 p-4 text-center">
              <p className="text-sm text-muted-foreground">
                {t("rss:rules.empty")}
              </p>
              <Button size="sm" onClick={newRule}>
                {t("rss:rules.create")}
              </Button>
            </div>
          ) : (
            rules.map((rule) => (
              <div
                key={rule.id}
                className={cn(
                  "flex w-full items-center gap-2 rounded px-2 py-1 text-sm hover:bg-muted",
                  selectedId === rule.id && "bg-accent text-accent-foreground",
                )}
              >
                <Checkbox
                  checked={rule.enabled}
                  onCheckedChange={(state) =>
                    void toggleEnabled(rule, state === true)
                  }
                  aria-label={t("rss:rules.enabled")}
                />
                <button
                  type="button"
                  className="min-w-0 flex-1 truncate text-left"
                  aria-pressed={selectedId === rule.id}
                  onClick={() => selectRule(rule)}
                >
                  {rule.name}
                </button>
              </div>
            ))
          )}
        </div>
        <div className="flex gap-2 border-t p-2">
          <Button
            size="sm"
            variant="outline"
            onClick={() => importInput.current?.click()}
          >
            {t("rss:rules.import")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={rules.length === 0}
            onClick={exportRules}
          >
            {t("rss:rules.export")}
          </Button>
          <input
            ref={importInput}
            type="file"
            accept=".json,application/json"
            hidden
            aria-hidden="true"
            tabIndex={-1}
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) void importRules(file);
              event.target.value = "";
            }}
          />
        </div>
      </div>

      <div className="min-w-0 flex-1 overflow-y-auto p-3">
        <div className="mx-auto max-w-xl space-y-3">
          <div className="space-y-1">
            <Label htmlFor="rule-name">{t("rss:rules.name")}</Label>
            <Input
              id="rule-name"
              value={doc.name}
              placeholder={t("rss:rules.namePlaceholder")}
              aria-label={t("rss:rules.name")}
              onChange={(event) =>
                update((draft) => (draft.name = event.target.value))
              }
            />
            {fieldAlert("name")}
          </div>

          <span className="flex items-center gap-2 text-sm">
            <Checkbox
              id="rule-enabled"
              checked={doc.enabled ?? true}
              onCheckedChange={(state) =>
                update((draft) => (draft.enabled = state === true))
              }
            />
            <Label htmlFor="rule-enabled">{t("rss:rules.enabled")}</Label>
          </span>

          <div className="flex items-center gap-2 text-sm">
            <Checkbox
              id="rule-regex"
              checked={doc.match.mode === "regex"}
              onCheckedChange={(state) =>
                update(
                  (draft) =>
                    (draft.match.mode = state === true ? "regex" : "wildcard"),
                )
              }
            />
            <Label htmlFor="rule-regex">{t("rss:rules.useRegex")}</Label>
            <Popover>
              <PopoverTrigger asChild>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={t("rss:rules.regexHelpTitle")}
                  className="h-6 w-6 p-0"
                >
                  <CircleHelp className="size-4" />
                </Button>
              </PopoverTrigger>
              <PopoverContent>
                <p className="font-medium">{t("rss:rules.regexHelpTitle")}</p>
                <p>{t("rss:rules.regexHelpRegex")}</p>
                <p>{t("rss:rules.regexHelpWildcard")}</p>
              </PopoverContent>
            </Popover>
            {fieldAlert("match.mode")}
          </div>

          <div className="space-y-1">
            <Label htmlFor="rule-any-of">{t("rss:rules.mustContain")}</Label>
            <textarea
              id="rule-any-of"
              rows={3}
              value={anyOfText}
              placeholder={t("rss:rules.clausePlaceholder")}
              onChange={(event) =>
                update(
                  (draft) =>
                    (draft.match.any_of = linesToPatterns(event.target.value)),
                )
              }
              className="flex w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-sm"
            />
            {fieldAlert("match.any_of")}
          </div>

          <div className="space-y-1">
            <Label htmlFor="rule-none-of">
              {t("rss:rules.mustNotContain")}
            </Label>
            <textarea
              id="rule-none-of"
              rows={3}
              value={noneOfText}
              placeholder={t("rss:rules.clausePlaceholder")}
              onChange={(event) =>
                update(
                  (draft) =>
                    (draft.match.none_of = linesToPatterns(event.target.value)),
                )
              }
              className="flex w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-sm"
            />
            {fieldAlert("match.none_of")}
          </div>

          <div className="space-y-1">
            <Label htmlFor="rule-episode-filter">
              {t("rss:rules.episodeFilter")}
            </Label>
            <Input
              id="rule-episode-filter"
              value={doc.episode?.filter ?? ""}
              placeholder={t("rss:rules.episodeFilterPlaceholder")}
              onChange={(event) =>
                update((draft) => {
                  const filter = event.target.value;
                  if (filter === "" && draft.episode?.smart !== true) {
                    draft.episode = undefined;
                  } else {
                    draft.episode = { ...draft.episode, filter };
                  }
                })
              }
            />
            {fieldAlert("episode.filter")}
          </div>

          <span className="flex items-center gap-2 text-sm">
            <Checkbox
              id="rule-smart"
              checked={doc.episode?.smart === true}
              onCheckedChange={(state) =>
                update((draft) => {
                  const smart = state === true;
                  if (!smart && (draft.episode?.filter ?? "") === "") {
                    draft.episode = undefined;
                  } else {
                    draft.episode = { ...draft.episode, smart };
                  }
                })
              }
            />
            <Label htmlFor="rule-smart">{t("rss:rules.smartEpisode")}</Label>
          </span>

          <div className="space-y-1">
            <Label>{t("rss:rules.applyToFeeds")}</Label>
            <div className="space-y-1 rounded-md border border-input p-2">
              {feeds.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  {t("rss:rules.allFeeds")}
                </p>
              ) : (
                feeds.map((feed) => {
                  const checked = (doc.feeds ?? []).includes(feed.url);
                  return (
                    <span
                      key={feed.id}
                      className="flex items-center gap-2 text-sm"
                    >
                      <Checkbox
                        id={`rule-feed-${feed.id}`}
                        checked={checked}
                        onCheckedChange={(state) =>
                          update((draft) => {
                            const current = draft.feeds ?? [];
                            const next =
                              state === true
                                ? [...current, feed.url]
                                : current.filter((url) => url !== feed.url);
                            draft.feeds = next.length > 0 ? next : undefined;
                          })
                        }
                      />
                      <Label htmlFor={`rule-feed-${feed.id}`}>
                        {feed.title ?? feed.url}
                      </Label>
                    </span>
                  );
                })
              )}
            </div>
          </div>

          <div className="space-y-1">
            <Label htmlFor="rule-destination">
              {t("rss:rules.destination")}
            </Label>
            <div className="flex gap-2">
              <Input
                id="rule-destination"
                value={doc.action.destination ?? ""}
                onChange={(event) =>
                  update(
                    (draft) => (draft.action.destination = event.target.value),
                  )
                }
              />
              <Button
                type="button"
                variant="outline"
                onClick={() => setBrowseOpen(true)}
              >
                {t("rss:rules.browse")}
              </Button>
            </div>
            {fieldAlert("action.destination")}
          </div>

          <div className="flex flex-wrap items-end gap-3">
            <div className="space-y-1">
              <Label htmlFor="rule-category">{t("rss:rules.category")}</Label>
              <Select
                value={
                  doc.action.category === undefined ||
                  doc.action.category === ""
                    ? CATEGORY_NONE
                    : doc.action.category
                }
                onValueChange={(value) =>
                  update(
                    (draft) =>
                      (draft.action.category =
                        value === CATEGORY_NONE ? undefined : value),
                  )
                }
              >
                <SelectTrigger id="rule-category" className="w-44">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={CATEGORY_NONE}>
                    {t("rss:rules.categoryNone")}
                  </SelectItem>
                  {categories.map((category) => (
                    <SelectItem key={category.name} value={category.name}>
                      {category.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1">
              <Label htmlFor="rule-tag">{t("rss:rules.tags")}</Label>
              <div className="flex items-center gap-2">
                <span className="flex flex-wrap gap-1">
                  {(doc.action.tags ?? []).map((tag) => (
                    <button
                      key={tag}
                      type="button"
                      title={tag}
                      className="rounded-md border px-1.5 py-0.5 text-xs hover:bg-muted"
                      onClick={() => removeTag(tag)}
                    >
                      {tag} ×
                    </button>
                  ))}
                </span>
                <Input
                  id="rule-tag"
                  value={tagDraft}
                  placeholder={t("rss:rules.tagPlaceholder")}
                  className="w-32"
                  onChange={(event) => setTagDraft(event.target.value)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") {
                      event.preventDefault();
                      addTag();
                    }
                  }}
                />
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={addTag}
                  disabled={tagDraft.trim() === ""}
                >
                  {t("rss:rules.addTag")}
                </Button>
              </div>
              {fieldAlert("action.tags")}
            </div>
          </div>

          <span className="flex items-center gap-2 text-sm">
            <Checkbox
              id="rule-paused"
              checked={doc.action.paused === true}
              onCheckedChange={(state) =>
                update((draft) => (draft.action.paused = state === true))
              }
            />
            <Label htmlFor="rule-paused">{t("rss:rules.addStopped")}</Label>
          </span>

          <div className="space-y-1">
            <Label htmlFor="rule-cooldown">{t("rss:rules.ignoreDays")}</Label>
            <Input
              id="rule-cooldown"
              type="number"
              min={0}
              className="w-28"
              value={doc.throttle?.cooldown_days ?? 0}
              onChange={(event) =>
                update((draft) => {
                  draft.throttle = {
                    ...draft.throttle,
                    cooldown_days: Math.max(
                      0,
                      Number.parseInt(event.target.value, 10) || 0,
                    ),
                  };
                })
              }
            />
            {fieldAlert("throttle.cooldown_days")}
          </div>

          <p className="text-sm text-muted-foreground">
            {selected?.last_match_at
              ? t("rss:rules.lastMatch", {
                  when: formatWhen(selected.last_match_at, undefined, locale),
                })
              : t("rss:rules.lastMatchNever")}
          </p>

          <div className="flex gap-2 pt-1">
            <Button
              onClick={() => void save()}
              disabled={saving || doc.name.trim() === "" || !dirty}
            >
              {t("rss:rules.save")}
            </Button>
            <Button
              variant="outline"
              disabled={selected === null}
              onClick={() => setRunConfirm(true)}
            >
              {t("rss:rules.runExisting")}
            </Button>
          </div>
        </div>
      </div>

      <div className="flex w-80 shrink-0 flex-col border-l">
        <div className="border-b p-3">
          <h2 className="text-sm font-semibold">
            {t("rss:rules.previewTitle")}
          </h2>
          <p className="text-sm text-muted-foreground">
            {t("rss:rules.previewHeadline", {
              matched: shownMatched,
              total: PREVIEW_LIMIT,
            })}
          </p>
          <span className="flex items-center gap-2 pt-1 text-sm">
            <Checkbox
              id="rule-ignore-state"
              checked={ignoreState}
              onCheckedChange={(state) => setIgnoreState(state === true)}
            />
            <Label htmlFor="rule-ignore-state">
              {t("rss:rules.ignoreDownloaded")}
            </Label>
          </span>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-2">
          {preview === null ? (
            <p className="p-2 text-sm text-muted-foreground">
              {pending
                ? t("rss:rules.previewLoading")
                : t("rss:rules.previewEmpty")}
            </p>
          ) : (
            <>
              <ul className="space-y-1">
                {shown.map((row, index) => (
                  <li
                    key={index}
                    className={cn(
                      "text-sm",
                      !row.matched && "text-muted-foreground",
                    )}
                  >
                    <span aria-hidden="true">{row.matched ? "✓ " : "✗ "}</span>
                    <span className="break-all">
                      <HighlightedTitle row={row} />
                    </span>
                    {!row.matched && row.reason !== undefined && (
                      <p className="pl-4 text-xs">
                        ↳{" "}
                        {t(`rss:rules.reason.${row.reason}`, {
                          detail: row.reason_detail ?? "",
                          defaultValue: row.reason_detail ?? row.reason,
                        })}
                      </p>
                    )}
                    {row.matched && row.matched_by !== undefined && (
                      <p className="pl-4 text-xs text-muted-foreground">
                        {Object.entries(row.matched_by).map(
                          ([clause, pattern]) => (
                            <span key={clause} className="mr-2">
                              {t("rss:rules.matchedClause", {
                                clause,
                                pattern,
                              })}
                            </span>
                          ),
                        )}
                      </p>
                    )}
                  </li>
                ))}
              </ul>
              {shown.length === 0 && (
                <p className="p-2 text-sm text-muted-foreground">
                  {t("rss:rules.previewEmpty")}
                </p>
              )}
              <p className="p-2 text-xs text-muted-foreground">
                {t("rss:rules.previewStats", {
                  evaluated: preview.evaluated,
                  matched: preview.matched,
                  ms: preview.elapsed_ms,
                })}
              </p>
            </>
          )}
        </div>

        <section
          aria-label={t("rss:rules.testTitle")}
          className="shrink-0 space-y-2 border-t p-3"
        >
          <h3 className="text-xs font-semibold uppercase text-muted-foreground">
            {t("rss:rules.testTitle")}
          </h3>
          <Input
            value={testTitle}
            placeholder={t("rss:rules.testTitle")}
            onChange={(event) => setTestTitle(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                void runTitleTest();
              }
            }}
          />
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              disabled={testBusy || testTitle.trim() === ""}
              onClick={() => void runTitleTest()}
            >
              {t("rss:rules.test")}
            </Button>
            {testResult !== null &&
              (testResult.matched ? (
                <span className="text-sm font-medium text-green-600">
                  {t("rss:rules.testMatch")}
                </span>
              ) : (
                <span className="text-sm font-medium text-destructive">
                  {t("rss:rules.testNoMatch")}
                </span>
              ))}
          </div>
          {testResult !== null && testResult.matched && (
            <p className="text-xs text-muted-foreground">
              {Object.entries(testResult.matched_by ?? {}).map(
                ([clause, pattern]) => (
                  <span key={clause} className="mr-2">
                    {t("rss:rules.matchedClause", { clause, pattern })}
                  </span>
                ),
              )}
            </p>
          )}
          {testResult !== null && !testResult.matched && (
            <p className="text-xs text-muted-foreground">
              {testResult.reason !== undefined &&
                t(`rss:rules.reason.${testResult.reason}`, {
                  detail: testResult.reason_detail ?? "",
                  defaultValue: testResult.reason_detail ?? testResult.reason,
                })}
            </p>
          )}
          {testError !== null && (
            <p role="alert" className="text-sm text-destructive">
              {testError}
            </p>
          )}
        </section>
      </div>

      <FolderBrowserDialog
        open={browseOpen}
        initialPath={doc.action.destination || undefined}
        onSelect={(path) =>
          update((draft) => (draft.action.destination = path))
        }
        onOpenChange={setBrowseOpen}
      />

      <Dialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("rss:rules.deleteTitle")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm">
            {t("rss:rules.deleteBody", { name: deleteTarget?.name ?? "" })}
          </p>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleteTarget(null)}>
              {t("rss:feeds.dialog.cancel")}
            </Button>
            <Button
              variant="destructive"
              onClick={() =>
                deleteTarget !== null && void removeRule(deleteTarget)
              }
            >
              {t("rss:rules.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={runConfirm} onOpenChange={setRunConfirm}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("rss:rules.runConfirmTitle")}</DialogTitle>
          </DialogHeader>
          <p className="text-sm">
            {t("rss:rules.runConfirmBody", { name: selected?.name ?? "" })}
          </p>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRunConfirm(false)}>
              {t("rss:feeds.dialog.cancel")}
            </Button>
            <Button onClick={() => void runExisting()}>
              {t("rss:rules.run")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
