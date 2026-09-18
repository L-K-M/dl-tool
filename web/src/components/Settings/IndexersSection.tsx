import { useState, type JSX } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDownIcon, ArrowUpIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatAbsolute, formatWhen } from "../../lib/format";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { IndexerDialog, type IndexerDialogState } from "./IndexerDialog";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type IndexerDTO = components["schemas"]["IndexerDTO"];

/** One row of GET /indexers, doc 05 §9.1. `api_key_set` is a boolean because the key itself is
 *  never returned; there is no field on this screen that could render it. */
export interface IndexerRow {
  id: string;
  name: string;
  kind: "torznab" | "newznab" | "dlsearch";
  enabled: boolean;
  url: string | null;
  api_key_set: boolean;
  definition_id: string | null;
  definition_source: string | null;
  provenance: string | null;
  legal_tier: string;
  priority: number;
  seeders_unknown: boolean;
  categories: { id: number; name: string }[];
  last_test_at: string | null;
  last_error: string | null;
}

/** POST /indexers/{id}/test, doc 05 §9.1. A reachable-but-broken indexer is a 200 with ok:false. */
export interface IndexerTestResult {
  ok: boolean;
  elapsed_ms: number;
  categories_found: number;
  server: string;
  error: string | null;
}

// The plan's canonical repository documentation; not a runtime setting.
const ADR_0010_URL =
  "https://github.com/L-K-M/dl-tool/blob/main/docs/decisions/0010-never-execute-third-party-definitions.md";

const INDEXERS_KEY = ["indexers"] as const;

function problemDetail(
  error: { detail?: string; title?: string; type?: string } | undefined,
): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

function toRow(dto: IndexerDTO): IndexerRow {
  return {
    ...dto,
    categories: (dto.categories ?? []).map(({ id, name }) => ({ id, name })),
  };
}

/** Rows render in the server's order — ORDER BY priority, name, lower value first.
 *  Move up swaps the two rows' priority values with one PATCH /indexers/{id} each and changes
 *  nothing else. It is a no-op on the first row, and on any row whose neighbour shares its priority. */
export function swapPriority(
  rows: IndexerRow[],
  id: string,
  dir: -1 | 1,
): { id: string; priority: number }[] {
  const index = rows.findIndex((row) => row.id === id);
  const row = index < 0 ? undefined : rows[index];
  const neighbour = index < 0 ? undefined : rows[index + dir];
  if (row === undefined || neighbour === undefined) return [];
  if (row.priority === neighbour.priority) return [];
  return [
    { id: row.id, priority: neighbour.priority },
    { id: neighbour.id, priority: row.priority },
  ];
}

export function IndexersSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();
  const indexers = useQuery({
    queryKey: INDEXERS_KEY,
    queryFn: async () => {
      const { data, error } = await api.GET("/indexers");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return (data.indexers ?? []).map(toRow);
    },
    retry: false,
  });
  const rows = indexers.data ?? [];
  const [dialog, setDialog] = useState<IndexerDialogState | null>(null);
  const [results, setResults] = useState<Record<string, IndexerTestResult>>({});
  const [pending, setPending] = useState<Record<string, boolean>>({});
  const [testingAll, setTestingAll] = useState(false);
  const [moving, setMoving] = useState(false);

  /** Exactly one capability probe; a 200 ok:false is rendered data, never a toast. */
  const probe = async (id: string): Promise<void> => {
    setPending((current) => ({ ...current, [id]: true }));
    try {
      const { data, error } = await api.POST("/indexers/{id}/test", {
        params: { path: { id } },
      });
      if (data !== undefined)
        setResults((current) => ({ ...current, [id]: data }));
      else
        toast.error(
          t("indexers.testRequestFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
    } catch {
      toast.error(
        t("indexers.testRequestFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setPending((current) => ({ ...current, [id]: false }));
    }
  };

  const testOne = async (id: string) => {
    await probe(id);
    // A probe can change last_test_at and last_error; refresh the row.
    void indexers.refetch();
  };

  const testAll = async () => {
    setTestingAll(true);
    try {
      for (const row of rows) await probe(row.id);
    } finally {
      setTestingAll(false);
      void indexers.refetch();
    }
  };

  const toggle = useMutation({
    mutationFn: async (change: { id: string; enabled: boolean }) => {
      const { data, error } = await api.PATCH("/indexers/{id}", {
        params: { path: { id: change.id } },
        body: { enabled: change.enabled },
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data;
    },
    onMutate: async (change) => {
      await queryClient.cancelQueries({ queryKey: INDEXERS_KEY });
      const previous = queryClient.getQueryData<IndexerRow[]>(INDEXERS_KEY);
      queryClient.setQueryData<IndexerRow[]>(INDEXERS_KEY, (old) =>
        old?.map((row) =>
          row.id === change.id ? { ...row, enabled: change.enabled } : row,
        ),
      );
      return { previous };
    },
    onError: (error, _change, context) => {
      if (context?.previous !== undefined)
        queryClient.setQueryData(INDEXERS_KEY, context.previous);
      toast.error(t("indexers.enableFailed", { detail: error.message }));
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: INDEXERS_KEY }),
  });

  /** A reorder is never optimistic: at most two PATCH calls, then a refetch. */
  const move = async (id: string, dir: -1 | 1) => {
    const patches = swapPriority(rows, id, dir);
    if (patches.length === 0) return;
    setMoving(true);
    try {
      for (const patch of patches) {
        const { error } = await api.PATCH("/indexers/{id}", {
          params: { path: { id: patch.id } },
          body: { priority: patch.priority },
        });
        if (error) {
          toast.error(
            t("indexers.moveFailed", {
              detail: problemDetail(error) ?? ct("shell.networkError"),
            }),
          );
          break;
        }
      }
    } catch {
      toast.error(
        t("indexers.moveFailed", { detail: ct("shell.networkError") }),
      );
    } finally {
      setMoving(false);
      void indexers.refetch();
    }
  };

  const loadError = (
    <p role="alert" className="text-sm text-destructive">
      {t("indexers.loadError")}{" "}
      <Button
        variant="outline"
        size="sm"
        onClick={() => void indexers.refetch()}
      >
        {ct("actions.retry")}
      </Button>
    </p>
  );

  // A failed background refetch keeps the previous data; only an error with
  // nothing cached replaces the whole section.
  if (indexers.isError && indexers.data === undefined) return loadError;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <Button size="sm" onClick={() => setDialog({ mode: "add" })}>
          {t("indexers.add")}
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={() => setDialog({ mode: "import" })}
        >
          {t("indexers.import")}
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={testingAll || rows.length === 0}
          onClick={() => void testAll()}
        >
          {testingAll ? t("indexers.testing") : t("indexers.testAll")}
        </Button>
      </div>
      {indexers.isError && loadError}
      <p className="text-xs text-muted-foreground">
        {t("indexers.note")}{" "}
        <a
          href={ADR_0010_URL}
          target="_blank"
          rel="noreferrer"
          className="underline underline-offset-2"
        >
          {t("indexers.adrLink")}
        </a>
      </p>
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-border text-start text-muted-foreground">
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colName")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colType")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colUrl")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colCategories")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colEnabled")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colPriority")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colLastTest")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("indexers.colActions")}
            </th>
          </tr>
        </thead>
        <tbody>
          {indexers.isLoading ? (
            <tr>
              <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                {t("indexers.loading")}
              </td>
            </tr>
          ) : rows.length === 0 ? (
            <tr>
              <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                {t("indexers.empty")}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const result = results[row.id];
              const toggling =
                toggle.isPending && toggle.variables?.id === row.id;
              const canMoveUp = swapPriority(rows, row.id, -1).length > 0;
              const canMoveDown = swapPriority(rows, row.id, 1).length > 0;
              const provenance =
                row.definition_source === "bundled"
                  ? null
                  : (row.provenance ?? row.definition_source);
              return (
                <tr
                  key={row.id}
                  className={`border-b border-border align-top ${
                    toggling ? "animate-pulse" : ""
                  }`}
                  aria-busy={toggling}
                >
                  <td className="px-2 py-2">
                    <span className="font-medium">{row.name}</span>
                    {provenance !== null && (
                      <p className="mt-1 text-xs text-muted-foreground">
                        {provenance}
                      </p>
                    )}
                    {row.last_error !== null && (
                      <p
                        className="mt-1 text-xs"
                        style={{ color: "var(--warn)" }}
                      >
                        {row.last_error}
                      </p>
                    )}
                    {result !== undefined && (
                      <dl
                        aria-live="polite"
                        className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground"
                      >
                        <div className="flex gap-1">
                          <dt>{t("indexers.resultOk")}</dt>
                          <dd>
                            {result.ok
                              ? t("indexers.resultPassed")
                              : t("indexers.resultFailed")}
                          </dd>
                        </div>
                        <div className="flex gap-1">
                          <dt>{t("indexers.resultElapsed")}</dt>
                          <dd>{result.elapsed_ms}</dd>
                        </div>
                        <div className="flex gap-1">
                          <dt>{t("indexers.resultCategories")}</dt>
                          <dd>{result.categories_found}</dd>
                        </div>
                        <div className="flex gap-1">
                          <dt>{t("indexers.resultServer")}</dt>
                          <dd>{result.server}</dd>
                        </div>
                        {result.error !== null && (
                          <div className="flex gap-1">
                            <dt>{t("indexers.resultError")}</dt>
                            <dd>{result.error}</dd>
                          </div>
                        )}
                      </dl>
                    )}
                  </td>
                  <td className="px-2 py-2">{row.kind}</td>
                  <td className="px-2 py-2">
                    {row.url === null ? (
                      "—"
                    ) : (
                      <span className="break-all" title={row.url}>
                        {row.url}
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-2">
                    <span
                      title={
                        row.categories.length === 0
                          ? undefined
                          : row.categories.map((c) => c.name).join(", ")
                      }
                    >
                      {row.categories.length}
                    </span>
                  </td>
                  <td className="px-2 py-2">
                    <Checkbox
                      checked={row.enabled}
                      disabled={toggling}
                      aria-label={t("indexers.enabledFor", {
                        name: row.name,
                      })}
                      onCheckedChange={(checked) =>
                        toggle.mutate({
                          id: row.id,
                          enabled: checked === true,
                        })
                      }
                    />
                  </td>
                  <td className="px-2 py-2">{row.priority}</td>
                  <td className="px-2 py-2">
                    {row.last_test_at === null ? (
                      t("indexers.neverTested")
                    ) : (
                      <span title={formatAbsolute(row.last_test_at)}>
                        {formatWhen(row.last_test_at)}
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-2">
                    <div className="flex items-center gap-1">
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={pending[row.id] === true || testingAll}
                        onClick={() => void testOne(row.id)}
                      >
                        {pending[row.id] === true
                          ? t("indexers.testing")
                          : t("indexers.test")}
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setDialog({ mode: "edit", indexer: row })
                        }
                      >
                        {t("indexers.edit")}
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={moving || !canMoveUp}
                        aria-label={t("indexers.moveUpFor", {
                          name: row.name,
                        })}
                        title={t("indexers.moveUpFor", { name: row.name })}
                        onClick={() => void move(row.id, -1)}
                      >
                        <ArrowUpIcon />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon-sm"
                        disabled={moving || !canMoveDown}
                        aria-label={t("indexers.moveDownFor", {
                          name: row.name,
                        })}
                        title={t("indexers.moveDownFor", { name: row.name })}
                        onClick={() => void move(row.id, 1)}
                      >
                        <ArrowDownIcon />
                      </Button>
                    </div>
                  </td>
                </tr>
              );
            })
          )}
        </tbody>
      </table>
      <IndexerDialog
        state={dialog}
        onClose={(changed) => {
          // A saved edit can change the URL or key, so a stale probe verdict
          // for the old configuration must not survive the refetch.
          if (changed && dialog?.mode === "edit") {
            const id = dialog.indexer.id;
            setResults((current) => {
              const next = { ...current };
              delete next[id];
              return next;
            });
          }
          setDialog(null);
          if (changed) void indexers.refetch();
        }}
      />
    </div>
  );
}
