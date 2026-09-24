import { useCallback, useEffect, useRef, useState, type JSX } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatAbsolute, formatWhen } from "../../lib/format";
import settingsStrings from "../../locales/en/settings.json";
import { FolderBrowserDialog } from "../FolderBrowser/FolderBrowserDialog";
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
import { Label } from "../ui/label";
import { useSettingsDirty } from "./SettingsScreen";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type ScanResult = components["schemas"]["ScanResult"];
type CreateWatchFolderBody =
  components["schemas"]["CreateWatchFolderInputBody"];
type PatchWatchFolderBody = components["schemas"]["PatchWatchFolderInputBody"];
type CreateCategoryBody = components["schemas"]["CreateCategoryInputBody"];
type PatchCategoryBody = components["schemas"]["PatchCategoryInputBody"];
type Problem = components["schemas"]["ErrorModel"];

/** The only settings keys this screen writes. Every one appears in
 *  docs/11-config-reference.md §5; PATCH /settings answers 422 for anything else. */
export const DOWNLOAD_KEYS = [
  "default_destination",
  "auto_extract",
  "extract_passwords",
  "min_free_space",
] as const;
export type DownloadKey = (typeof DOWNLOAD_KEYS)[number];

/** One row of GET /watch-folders, doc 05 §15. */
export interface WatchFolderRow {
  id: string;
  path: string;
  enabled: boolean;
  destination: string;
  category: string | null;
  delete_after_load: boolean;
  poll_interval_s: number;
  last_scan_at: string | null;
  last_error: string | null;
}

/** One row of GET /categories, doc 05 §8.1. */
export interface CategoryRow {
  name: string;
  save_path: string;
  task_count: number;
}

/** The four members of GET /settings this screen reads, doc 11 §5.
 *  extract_passwords only ever arrives as the "__redacted__" placeholder —
 *  the stored list is never returned, so there is nothing here to render. */
interface DownloadSettings {
  default_destination: string;
  auto_extract: boolean;
  extract_passwords: string;
  min_free_space: Record<string, number>;
}

// Doc 11 §5's floor for a root absent from the stored sparse map.
const DEFAULT_MIN_FREE_SPACE = 2147483648;

const SETTINGS_KEY = ["settings"] as const;
const ROOTS_KEY = ["fs-roots"] as const;
const WATCH_FOLDERS_KEY = ["watch-folders"] as const;
const CATEGORIES_KEY = ["categories"] as const;

const selectClass =
  "h-8 w-full rounded-lg border border-input bg-transparent px-2 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 outline-none";

function problemDetail(error: Problem | undefined): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

function isPathRejected(error: Problem | undefined): boolean {
  return error?.type === "/problems/path-rejected";
}

/** The extract_passwords half of the form. The stored list is a write-only
 *  secret: untouched sends nothing, replace sends the whole new list and
 *  clear sends an empty array. The placeholder is never sent back. */
type PasswordState =
  { mode: "untouched" } | { mode: "replace"; text: string } | { mode: "clear" };

/** Editable state, held as the inputs' own text so an in-progress edit
 *  round-trips without reformatting. */
interface FormState {
  defaultDestination: string;
  autoExtract: boolean;
  passwords: PasswordState;
  minFreeSpace: Record<string, string>;
}

function seedForm(settings: DownloadSettings, roots: string[]): FormState {
  const minFreeSpace: Record<string, string> = {};
  for (const root of roots)
    minFreeSpace[root] = String(
      settings.min_free_space[root] ?? DEFAULT_MIN_FREE_SPACE,
    );
  return {
    defaultDestination: settings.default_destination,
    autoExtract: settings.auto_extract,
    passwords: { mode: "untouched" },
    minFreeSpace,
  };
}

function mapsEqual(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const key of keys) if (a[key] !== b[key]) return false;
  return true;
}

/** One password per line; blank lines are not passwords. Interior spaces are
 *  kept — "correct horse battery" is a legal entry. */
function passwordLines(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.replace(/\r$/, ""))
    .filter((line) => line !== "");
}

/** A path that only FolderBrowserDialog can write: the input is read-only
 *  text and the Browse button opens the server-side browser. */
function PathField({
  id,
  label,
  value,
  error,
  browseText,
  browseLabel,
  onBrowse,
}: {
  id: string;
  /** Rendered above the input; omitted when the caller places its own Label. */
  label?: string;
  value: string;
  error?: string;
  browseText: string;
  /** Accessible name of the Browse button, naming the path it chooses. */
  browseLabel: string;
  onBrowse: () => void;
}): JSX.Element {
  return (
    <div className="flex flex-col gap-1">
      {label !== undefined && <Label htmlFor={id}>{label}</Label>}
      <div className="flex items-center gap-1.5">
        <Input
          id={id}
          readOnly
          value={value}
          className="w-72"
          aria-invalid={error !== undefined || undefined}
          aria-describedby={error !== undefined ? `${id}-error` : undefined}
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-label={browseLabel}
          onClick={onBrowse}
        >
          {browseText}
        </Button>
      </div>
      {error !== undefined && (
        <p id={`${id}-error`} role="alert" className="text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}

type WatchFolderDialogState =
  { mode: "add" } | { mode: "edit"; folder: WatchFolderRow };

type CategoryDialogState =
  { mode: "add" } | { mode: "edit"; category: CategoryRow };

type DeleteTarget =
  | { kind: "watch"; row: WatchFolderRow }
  | { kind: "category"; row: CategoryRow };

export function DownloadsSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();

  const settings = useQuery({
    queryKey: SETTINGS_KEY,
    queryFn: async (): Promise<DownloadSettings> => {
      const { data, error } = await api.GET("/settings");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return {
        default_destination: data.default_destination,
        auto_extract: data.auto_extract,
        extract_passwords: data.extract_passwords,
        min_free_space: data.min_free_space ?? {},
      };
    },
    retry: false,
  });
  const roots = useQuery({
    queryKey: ROOTS_KEY,
    queryFn: async (): Promise<string[]> => {
      const { data, error } = await api.GET("/fs/roots");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return (data.roots ?? []).map((root) => root.path);
    },
    retry: false,
  });
  const watchFolders = useQuery({
    queryKey: WATCH_FOLDERS_KEY,
    queryFn: async (): Promise<WatchFolderRow[]> => {
      const { data, error } = await api.GET("/watch-folders");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return (data.watch_folders ?? []).map((dto) => ({
        id: dto.id,
        path: dto.path,
        enabled: dto.enabled,
        destination: dto.destination,
        category: dto.category,
        delete_after_load: dto.delete_after_load,
        poll_interval_s: dto.poll_interval_s,
        last_scan_at: dto.last_scan_at,
        last_error: dto.last_error,
      }));
    },
    retry: false,
  });
  const categories = useQuery({
    queryKey: CATEGORIES_KEY,
    queryFn: async (): Promise<CategoryRow[]> => {
      const { data, error } = await api.GET("/categories");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data.categories ?? [];
    },
    retry: false,
  });

  const ready = settings.data !== undefined && roots.data !== undefined;

  const [form, setForm] = useState<FormState | null>(null);
  const [baseline, setBaseline] = useState<FormState | null>(null);
  // The stored sparse map, kept so a save can merge displayed roots over it
  // without dropping entries for roots no longer configured (doc 11 §5).
  const [storedMap, setStoredMap] = useState<Record<string, number>>({});
  const [destError, setDestError] = useState<string | null>(null);
  const [spaceErrors, setSpaceErrors] = useState<Record<string, string>>({});
  const [browseDestination, setBrowseDestination] = useState(false);
  const [wfDialog, setWfDialog] = useState<WatchFolderDialogState | null>(null);
  const [catDialog, setCatDialog] = useState<CategoryDialogState | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget | null>(null);
  const [scanning, setScanning] = useState<Record<string, boolean>>({});
  const [scanResults, setScanResults] = useState<Record<string, ScanResult>>(
    {},
  );
  const savingRef = useRef(false);

  const formRef = useRef(form);
  formRef.current = form;
  const baselineRef = useRef(baseline);
  baselineRef.current = baseline;
  const storedMapRef = useRef(storedMap);
  storedMapRef.current = storedMap;

  // The seed runs once, on the first render with both responses; a later
  // refetch refreshes the queries, not the in-progress form.
  useEffect(() => {
    if (!ready || form !== null) return;
    const seeded = seedForm(
      settings.data as DownloadSettings,
      roots.data as string[],
    );
    setForm(seeded);
    setBaseline({ ...seeded, minFreeSpace: { ...seeded.minFreeSpace } });
    setStoredMap((settings.data as DownloadSettings).min_free_space);
  }, [ready, form, settings.data, roots.data]);

  const dirtyCount =
    form === null || baseline === null
      ? 0
      : [
          form.defaultDestination !== baseline.defaultDestination,
          form.autoExtract !== baseline.autoExtract,
          form.passwords.mode !== "untouched",
          !mapsEqual(form.minFreeSpace, baseline.minFreeSpace),
        ].filter(Boolean).length;

  /** One PATCH /settings carrying only the changed DOWNLOAD_KEYS members. A
   *  rejected call ends the save with the form still dirty; a path-rejected
   *  403 lands on the destination field, the only path member this screen
   *  can send. */
  const save = useCallback(() => {
    const current = formRef.current;
    const base = baselineRef.current;
    const stored = storedMapRef.current;
    if (current === null || base === null || savingRef.current) return;
    savingRef.current = true;
    void (async () => {
      try {
        const body: {
          default_destination?: string;
          auto_extract?: boolean;
          extract_passwords?: string[];
          min_free_space?: Record<string, number>;
        } = {};
        if (current.defaultDestination !== base.defaultDestination)
          body.default_destination = current.defaultDestination;
        if (current.autoExtract !== base.autoExtract)
          body.auto_extract = current.autoExtract;
        if (current.passwords.mode === "replace")
          body.extract_passwords = passwordLines(current.passwords.text);
        else if (current.passwords.mode === "clear")
          body.extract_passwords = [];
        if (!mapsEqual(current.minFreeSpace, base.minFreeSpace)) {
          const errors: Record<string, string> = {};
          // The stored map merges under the displayed values, so entries
          // for roots no longer configured are preserved, not dropped.
          const map: Record<string, number> = { ...stored };
          for (const [root, text] of Object.entries(current.minFreeSpace)) {
            const value = Number(text);
            // Anything but a non-negative whole number is a guaranteed 422,
            // so it fails on the field now instead of shipping a doomed PATCH.
            if (text.trim() === "" || !Number.isInteger(value) || value < 0)
              errors[root] = t("downloads.invalidBytes");
            else map[root] = value;
          }
          if (Object.keys(errors).length > 0) {
            setSpaceErrors(errors);
            return;
          }
          body.min_free_space = map;
        }

        const { error } = await api.PATCH("/settings", { body });
        if (error !== undefined) {
          if (isPathRejected(error))
            setDestError(problemDetail(error) ?? ct("shell.networkError"));
          else
            toast.error(
              t("downloads.saveFailed", {
                detail: problemDetail(error) ?? ct("shell.networkError"),
              }),
            );
          return;
        }
        setBaseline({
          ...current,
          passwords: { mode: "untouched" },
          minFreeSpace: { ...current.minFreeSpace },
        });
        if (body.min_free_space !== undefined)
          setStoredMap({ ...body.min_free_space });
        if (current.passwords.mode !== "untouched")
          setForm((prev) =>
            prev === null
              ? prev
              : { ...prev, passwords: { mode: "untouched" } },
          );
        setDestError(null);
        setSpaceErrors({});
        await queryClient.invalidateQueries({ queryKey: SETTINGS_KEY });
      } catch (error) {
        // openapi-fetch throws rather than returning `error` on a
        // network-level failure; surface it like a rejected response.
        toast.error(
          t("downloads.saveFailed", {
            detail:
              error instanceof Error ? error.message : ct("shell.networkError"),
          }),
        );
      } finally {
        savingRef.current = false;
      }
    })();
  }, [queryClient, t, ct]);

  const revert = useCallback(() => {
    const base = baselineRef.current;
    setForm(
      base === null
        ? null
        : {
            ...base,
            passwords: { mode: "untouched" },
            minFreeSpace: { ...base.minFreeSpace },
          },
    );
    setDestError(null);
    setSpaceErrors({});
    // Task step 3: Revert refetches GET /settings so the cache does not keep
    // serving a stale seed to the next mount.
    void queryClient.invalidateQueries({ queryKey: SETTINGS_KEY });
  }, [queryClient]);

  useEffect(() => {
    useSettingsDirty.setState((prev) => {
      if (dirtyCount === 0)
        return prev.report === null ? prev : { report: null };
      if (
        prev.report?.count === dirtyCount &&
        prev.report.section === "downloads"
      )
        return prev;
      return {
        report: { section: "downloads", count: dirtyCount, save, revert },
      };
    });
  }, [dirtyCount, save, revert]);
  useEffect(() => () => useSettingsDirty.setState({ report: null }), []);

  /** A row-level PATCH /watch-folders/{id} — the enabled and
   *  delete-after-load toggles write immediately, like the indexer toggle. */
  const patchFolder = async (id: string, body: PatchWatchFolderBody) => {
    try {
      const { error } = await api.PATCH("/watch-folders/{id}", {
        params: { path: { id } },
        body,
      });
      if (error !== undefined) {
        toast.error(
          t("downloads.wfSaveFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
        return;
      }
    } catch {
      toast.error(
        t("downloads.wfSaveFailed", { detail: ct("shell.networkError") }),
      );
      return;
    }
    await watchFolders.refetch();
  };

  const scan = async (id: string) => {
    setScanning((current) => ({ ...current, [id]: true }));
    try {
      const { data, error } = await api.POST("/watch-folders/{id}/scan", {
        params: { path: { id } },
      });
      if (data !== undefined)
        setScanResults((current) => ({ ...current, [id]: data }));
      else
        toast.error(
          t("downloads.wfScanFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
    } catch {
      toast.error(
        t("downloads.wfScanFailed", { detail: ct("shell.networkError") }),
      );
    } finally {
      setScanning((current) => ({ ...current, [id]: false }));
      // The scan rewrites last_scan_at and last_error on the row.
      void watchFolders.refetch();
    }
  };

  const removeTarget = async () => {
    const target = deleteTarget;
    if (target === null) return;
    try {
      const { error } =
        target.kind === "watch"
          ? await api.DELETE("/watch-folders/{id}", {
              params: { path: { id: target.row.id } },
            })
          : await api.DELETE("/categories/{name}", {
              params: { path: { name: target.row.name } },
            });
      if (error !== undefined) {
        toast.error(
          t(
            target.kind === "watch"
              ? "downloads.wfDeleteFailed"
              : "downloads.catDeleteFailed",
            { detail: problemDetail(error) ?? ct("shell.networkError") },
          ),
        );
        return;
      }
    } catch {
      toast.error(
        t(
          target.kind === "watch"
            ? "downloads.wfDeleteFailed"
            : "downloads.catDeleteFailed",
          { detail: ct("shell.networkError") },
        ),
      );
      return;
    }
    setDeleteTarget(null);
    if (target.kind === "watch") await watchFolders.refetch();
    else await categories.refetch();
  };

  // A failed background refetch keeps the previous data; only an error with
  // nothing cached replaces the whole section.
  const failed = [settings, roots].some(
    (query) => query.isError && query.data === undefined,
  );
  if (failed)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("downloads.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            void settings.refetch();
            void roots.refetch();
          }}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );
  if (form === null) return <></>;

  const folderRows = watchFolders.data ?? [];
  const categoryRows = categories.data ?? [];

  return (
    <div className="flex flex-col gap-6">
      <div className="flex max-w-xl flex-col gap-5">
        <div className="flex items-start justify-between gap-4">
          <div className="pt-1.5">
            <Label htmlFor="dl-destination">
              {t("downloads.defaultDestination")}
            </Label>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("downloads.destinationHint")}
            </p>
          </div>
          <PathField
            id="dl-destination"
            value={form.defaultDestination}
            error={destError ?? undefined}
            browseText={t("downloads.browse")}
            browseLabel={t("downloads.browseDestination")}
            onBrowse={() => setBrowseDestination(true)}
          />
        </div>

        <div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="dl-auto-extract">
              {t("downloads.autoExtract")}
            </Label>
            <Checkbox
              id="dl-auto-extract"
              checked={form.autoExtract}
              onCheckedChange={(checked) =>
                setForm({ ...form, autoExtract: checked === true })
              }
            />
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {t("downloads.autoExtractHint")}
          </p>
        </div>

        <div>
          <p className="text-sm font-medium">{t("downloads.passwordList")}</p>
          {form.passwords.mode === "replace" ? (
            <div className="mt-1 flex flex-col gap-1">
              <Label htmlFor="dl-passwords" className="sr-only">
                {t("downloads.passwordEditLabel")}
              </Label>
              <textarea
                id="dl-passwords"
                rows={4}
                spellCheck={false}
                autoComplete="off"
                value={form.passwords.text}
                onChange={(event) =>
                  setForm({
                    ...form,
                    passwords: { mode: "replace", text: event.target.value },
                  })
                }
                className="flex w-full rounded-md border border-input bg-transparent px-2 py-1 font-mono text-sm"
              />
              <p className="text-xs text-muted-foreground">
                {t("downloads.passwordEditHint")}
              </p>
            </div>
          ) : (
            <div className="mt-1 flex items-center gap-2">
              <p className="text-sm text-muted-foreground">
                {form.passwords.mode === "clear"
                  ? t("downloads.passwordWillClear")
                  : t("downloads.passwordStored")}
              </p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() =>
                  setForm({
                    ...form,
                    passwords: { mode: "replace", text: "" },
                  })
                }
              >
                {t("downloads.passwordReplace")}
              </Button>
              {form.passwords.mode === "untouched" && (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    setForm({ ...form, passwords: { mode: "clear" } })
                  }
                >
                  {t("downloads.passwordClear")}
                </Button>
              )}
            </div>
          )}
        </div>

        <div>
          <p className="text-sm font-medium">{t("downloads.minFreeSpace")}</p>
          <p className="mt-1 text-xs text-muted-foreground">
            {t("downloads.minFreeSpaceHint")}
          </p>
          <div className="mt-2 flex flex-col gap-2">
            {(roots.data ?? []).map((root) => (
              <div key={root} className="flex items-center gap-3">
                <Label
                  htmlFor={`dl-free-${root}`}
                  className="w-56 shrink-0 truncate font-mono text-xs"
                  title={root}
                >
                  {root}
                </Label>
                <Input
                  id={`dl-free-${root}`}
                  type="number"
                  min={0}
                  step={1}
                  className="w-40"
                  value={form.minFreeSpace[root] ?? ""}
                  aria-invalid={spaceErrors[root] !== undefined || undefined}
                  onChange={(event) => {
                    setForm({
                      ...form,
                      minFreeSpace: {
                        ...form.minFreeSpace,
                        [root]: event.target.value,
                      },
                    });
                    // Editing clears the error outright — the key is
                    // deleted, so aria-invalid and the message agree.
                    setSpaceErrors((prev) => {
                      const next = { ...prev };
                      delete next[root];
                      return next;
                    });
                  }}
                />
                {spaceErrors[root] !== undefined && (
                  <p role="alert" className="text-xs text-destructive">
                    {spaceErrors[root]}
                  </p>
                )}
              </div>
            ))}
          </div>
        </div>
      </div>

      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between gap-4">
          <div>
            <h2 className="text-sm font-medium">
              {t("downloads.watchFolders")}
            </h2>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("downloads.watchFoldersHint")}
            </p>
          </div>
          <Button
            size="sm"
            aria-label={t("downloads.wfAddLabel")}
            onClick={() => setWfDialog({ mode: "add" })}
          >
            {t("downloads.wfAdd")}
          </Button>
        </div>
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-start text-muted-foreground">
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColPath")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColDestination")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColCategory")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColDeleteLoaded")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColEnabled")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColLastScan")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColLastError")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.wfColActions")}
              </th>
            </tr>
          </thead>
          <tbody>
            {watchFolders.isLoading ? (
              <tr>
                <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                  {t("downloads.wfLoading")}
                </td>
              </tr>
            ) : watchFolders.isError ? (
              <tr>
                <td
                  colSpan={8}
                  role="alert"
                  className="px-2 py-2 text-destructive"
                >
                  {t("downloads.loadError")}
                </td>
              </tr>
            ) : folderRows.length === 0 ? (
              <tr>
                <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                  {t("downloads.wfEmpty")}
                </td>
              </tr>
            ) : (
              folderRows.map((row) => {
                const result = scanResults[row.id];
                return (
                  <tr key={row.id} className="border-b border-border align-top">
                    <td className="px-2 py-2">
                      <span className="break-all font-mono text-xs">
                        {row.path}
                      </span>
                      {result !== undefined && (
                        <div
                          aria-live="polite"
                          className="mt-1 text-xs text-muted-foreground"
                        >
                          <p>
                            {t("downloads.wfScanSummary", {
                              scanned: result.scanned,
                              created: (result.created ?? []).length,
                            })}
                          </p>
                          {(result.skipped ?? []).map((skip) => (
                            <p key={skip.file}>
                              {skip.file} — {skip.reason}
                            </p>
                          ))}
                        </div>
                      )}
                    </td>
                    <td className="px-2 py-2">
                      <span className="break-all font-mono text-xs">
                        {row.destination}
                      </span>
                    </td>
                    <td className="px-2 py-2">{row.category ?? "—"}</td>
                    <td className="px-2 py-2">
                      <Checkbox
                        checked={row.delete_after_load}
                        aria-label={t("downloads.wfToggleDeleteLoaded", {
                          path: row.path,
                        })}
                        onCheckedChange={(checked) =>
                          void patchFolder(row.id, {
                            delete_after_load: checked === true,
                          })
                        }
                      />
                    </td>
                    <td className="px-2 py-2">
                      <Checkbox
                        checked={row.enabled}
                        aria-label={t("downloads.wfToggleEnabled", {
                          path: row.path,
                        })}
                        onCheckedChange={(checked) =>
                          void patchFolder(row.id, {
                            enabled: checked === true,
                          })
                        }
                      />
                    </td>
                    <td className="px-2 py-2">
                      {row.last_scan_at === null ? (
                        t("downloads.wfNeverScanned")
                      ) : (
                        <span title={formatAbsolute(row.last_scan_at)}>
                          {formatWhen(row.last_scan_at)}
                        </span>
                      )}
                    </td>
                    <td className="px-2 py-2">
                      {row.last_error !== null && (
                        <span
                          className="text-xs"
                          style={{ color: "var(--warn)" }}
                        >
                          {row.last_error}
                        </span>
                      )}
                    </td>
                    <td className="px-2 py-2">
                      <div className="flex items-center gap-1">
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={scanning[row.id] === true}
                          onClick={() => void scan(row.id)}
                        >
                          {scanning[row.id] === true
                            ? t("downloads.wfScanning")
                            : t("downloads.wfScan")}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() =>
                            setWfDialog({ mode: "edit", folder: row })
                          }
                        >
                          {t("downloads.wfEdit")}
                        </Button>
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() =>
                            setDeleteTarget({ kind: "watch", row })
                          }
                        >
                          {t("downloads.wfDelete")}
                        </Button>
                      </div>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between gap-4">
          <div>
            <h2 className="text-sm font-medium">{t("downloads.categories")}</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("downloads.categoriesHint")}
            </p>
          </div>
          <Button
            size="sm"
            aria-label={t("downloads.catAddLabel")}
            onClick={() => setCatDialog({ mode: "add" })}
          >
            {t("downloads.catAdd")}
          </Button>
        </div>
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-start text-muted-foreground">
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.catColName")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.catColSavePath")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.catColTasks")}
              </th>
              <th scope="col" className="px-2 py-1.5 text-start font-medium">
                {t("downloads.catColActions")}
              </th>
            </tr>
          </thead>
          <tbody>
            {categories.isLoading ? (
              <tr>
                <td colSpan={4} className="px-2 py-2 text-muted-foreground">
                  {t("downloads.catLoading")}
                </td>
              </tr>
            ) : categories.isError ? (
              <tr>
                <td
                  colSpan={4}
                  role="alert"
                  className="px-2 py-2 text-destructive"
                >
                  {t("downloads.loadError")}
                </td>
              </tr>
            ) : categoryRows.length === 0 ? (
              <tr>
                <td colSpan={4} className="px-2 py-2 text-muted-foreground">
                  {t("downloads.catEmpty")}
                </td>
              </tr>
            ) : (
              categoryRows.map((row) => (
                <tr key={row.name} className="border-b border-border align-top">
                  <td className="px-2 py-2 font-medium">{row.name}</td>
                  <td className="px-2 py-2">
                    <span className="break-all font-mono text-xs">
                      {row.save_path}
                    </span>
                  </td>
                  <td className="px-2 py-2">{row.task_count}</td>
                  <td className="px-2 py-2">
                    <div className="flex items-center gap-1">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setCatDialog({ mode: "edit", category: row })
                        }
                      >
                        {t("downloads.catEdit")}
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setDeleteTarget({ kind: "category", row })
                        }
                      >
                        {t("downloads.catDelete")}
                      </Button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      <FolderBrowserDialog
        open={browseDestination}
        initialPath={form.defaultDestination}
        onSelect={(path) => {
          setForm((prev) =>
            prev === null ? prev : { ...prev, defaultDestination: path },
          );
          setDestError(null);
        }}
        onOpenChange={setBrowseDestination}
      />

      <WatchFolderDialog
        state={wfDialog}
        categories={categoryRows}
        onClose={(changed) => {
          setWfDialog(null);
          if (changed) void watchFolders.refetch();
        }}
      />
      <CategoryDialog
        state={catDialog}
        onClose={(changed) => {
          setCatDialog(null);
          if (changed) void categories.refetch();
        }}
      />
      <ConfirmDeleteDialog
        target={deleteTarget}
        onCancel={() => setDeleteTarget(null)}
        onConfirm={() => void removeTarget()}
      />
    </div>
  );
}

/** Add/edit one watch folder. Path and destination are read-only text — the
 *  only writer is FolderBrowserDialog, so a path the roots would reject can
 *  never be typed. */
function WatchFolderDialog({
  state,
  categories,
  onClose,
}: {
  state: WatchFolderDialogState | null;
  categories: CategoryRow[];
  onClose: (changed: boolean) => void;
}): JSX.Element | null {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const editing = state?.mode === "edit" ? state.folder : null;
  const [path, setPath] = useState("");
  const [destination, setDestination] = useState("");
  const [category, setCategory] = useState("");
  const [deleteLoaded, setDeleteLoaded] = useState(false);
  const [enabled, setEnabled] = useState(true);
  const [pollInterval, setPollInterval] = useState("10");
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [browse, setBrowse] = useState<"path" | "destination" | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setPath(editing?.path ?? "");
    setDestination(editing?.destination ?? "");
    setCategory(editing?.category ?? "");
    setDeleteLoaded(editing?.delete_after_load ?? false);
    setEnabled(editing?.enabled ?? true);
    setPollInterval(String(editing?.poll_interval_s ?? 10));
    setFieldErrors({});
    setBrowse(null);
    // The form reseeds whenever a different dialog state opens.
  }, [state, editing]);

  if (state === null) return null;
  const title =
    state.mode === "add"
      ? t("downloads.wfDialog.addTitle")
      : t("downloads.wfDialog.editTitle");

  const submit = async () => {
    const interval = Number(pollInterval);
    // Path and destination are only ever browser-picked; the disabled Save
    // covers an empty one. The interval is the one free-typed number.
    if (
      pollInterval.trim() === "" ||
      !Number.isInteger(interval) ||
      interval < 1
    ) {
      setFieldErrors({ poll: t("downloads.invalidInterval") });
      return;
    }
    setFieldErrors({});
    setBusy(true);
    try {
      if (state.mode === "add") {
        const body: CreateWatchFolderBody = {
          path,
          destination,
          enabled,
          delete_after_load: deleteLoaded,
          poll_interval_s: interval,
        };
        if (category !== "") body.category = category;
        const { error } = await api.POST("/watch-folders", { body });
        if (error !== undefined) {
          report(error);
          return;
        }
      } else {
        const body: PatchWatchFolderBody = {};
        if (path !== editing?.path) body.path = path;
        if (destination !== editing?.destination)
          body.destination = destination;
        if (category !== (editing?.category ?? ""))
          body.category = category === "" ? null : category;
        if (deleteLoaded !== editing?.delete_after_load)
          body.delete_after_load = deleteLoaded;
        if (enabled !== editing?.enabled) body.enabled = enabled;
        if (interval !== editing?.poll_interval_s)
          body.poll_interval_s = interval;
        // An unchanged form would send an empty PATCH; just close.
        if (Object.keys(body).length === 0) {
          onClose(false);
          return;
        }
        const { error } = await api.PATCH("/watch-folders/{id}", {
          params: { path: { id: state.folder.id } },
          body,
        });
        if (error !== undefined) {
          report(error);
          return;
        }
      }
      onClose(true);
    } catch {
      toast.error(
        t("downloads.wfSaveFailed", { detail: ct("shell.networkError") }),
      );
    } finally {
      setBusy(false);
    }
  };

  /** A 403 /problems/path-rejected lands on the field whose value the detail
   *  names; anything else is a toast. */
  const report = (error: Problem | undefined) => {
    const detail = problemDetail(error) ?? ct("shell.networkError");
    if (isPathRejected(error)) {
      // The detail is matched against the destination only when the
      // destination was actually edited — an unchanged value that happens
      // to appear in the message must not steal the error from the path.
      const destinationChanged =
        state.mode === "add" || destination !== editing?.destination;
      const field =
        destinationChanged && destination !== "" && detail.includes(destination)
          ? "destination"
          : "path";
      setFieldErrors((prev) => ({ ...prev, [field]: detail }));
      return;
    }
    toast.error(t("downloads.wfSaveFailed", { detail }));
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose(false);
      }}
    >
      <DialogContent className="sm:max-w-md" aria-label={title}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          <PathField
            id="wf-path"
            label={t("downloads.wfDialog.path")}
            value={path}
            error={fieldErrors.path}
            browseText={t("downloads.browse")}
            browseLabel={t("downloads.wfDialog.browsePath")}
            onBrowse={() => setBrowse("path")}
          />
          <PathField
            id="wf-destination"
            label={t("downloads.wfDialog.destination")}
            value={destination}
            error={fieldErrors.destination}
            browseText={t("downloads.browse")}
            browseLabel={t("downloads.wfDialog.browseDestination")}
            onBrowse={() => setBrowse("destination")}
          />
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="wf-category">
              {t("downloads.wfDialog.category")}
            </Label>
            <select
              id="wf-category"
              className={selectClass + " w-64"}
              value={category}
              onChange={(event) => setCategory(event.target.value)}
            >
              <option value="">{t("downloads.wfDialog.categoryNone")}</option>
              {categories.map((row) => (
                <option key={row.name} value={row.name}>
                  {row.name}
                </option>
              ))}
            </select>
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="wf-delete-loaded">
              {t("downloads.wfDialog.deleteLoaded")}
            </Label>
            <Checkbox
              id="wf-delete-loaded"
              checked={deleteLoaded}
              onCheckedChange={(checked) => setDeleteLoaded(checked === true)}
            />
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="wf-enabled">
              {t("downloads.wfDialog.enabled")}
            </Label>
            <Checkbox
              id="wf-enabled"
              checked={enabled}
              onCheckedChange={(checked) => setEnabled(checked === true)}
            />
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="wf-poll">
              {t("downloads.wfDialog.pollInterval")}
            </Label>
            <div className="flex flex-col gap-1">
              <Input
                id="wf-poll"
                type="number"
                min={1}
                step={1}
                className="w-28"
                value={pollInterval}
                aria-invalid={fieldErrors.poll !== undefined || undefined}
                onChange={(event) => setPollInterval(event.target.value)}
              />
              {fieldErrors.poll !== undefined && (
                <p role="alert" className="text-xs text-destructive">
                  {fieldErrors.poll}
                </p>
              )}
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => onClose(false)}
          >
            {t("downloads.wfDialog.cancel")}
          </Button>
          <Button
            disabled={busy || path === "" || destination === ""}
            onClick={() => void submit()}
          >
            {t("downloads.wfDialog.save")}
          </Button>
        </DialogFooter>
        <FolderBrowserDialog
          open={browse !== null}
          initialPath={browse === "destination" ? destination : path}
          onSelect={(selected) => {
            if (browse === "destination") setDestination(selected);
            else if (browse === "path") setPath(selected);
            setBrowse(null);
          }}
          onOpenChange={(open) => {
            if (!open) setBrowse(null);
          }}
        />
      </DialogContent>
    </Dialog>
  );
}

/** Add/edit one category. The save path is read-only text written only by
 *  FolderBrowserDialog; the name is the one free-text field. */
function CategoryDialog({
  state,
  onClose,
}: {
  state: CategoryDialogState | null;
  onClose: (changed: boolean) => void;
}): JSX.Element | null {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const editing = state?.mode === "edit" ? state.category : null;
  const [name, setName] = useState("");
  const [savePath, setSavePath] = useState("");
  const [pathError, setPathError] = useState<string | null>(null);
  const [browse, setBrowse] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setName(editing?.name ?? "");
    setSavePath(editing?.save_path ?? "");
    setPathError(null);
    setBrowse(false);
  }, [state, editing]);

  if (state === null) return null;
  const title =
    state.mode === "add"
      ? t("downloads.catDialog.addTitle")
      : t("downloads.catDialog.editTitle");

  const submit = async () => {
    const trimmed = name.trim();
    setBusy(true);
    try {
      if (state.mode === "add") {
        const body: CreateCategoryBody = {
          name: trimmed,
          save_path: savePath,
        };
        const { error } = await api.POST("/categories", { body });
        if (error !== undefined) {
          report(error);
          return;
        }
      } else {
        const body: PatchCategoryBody = {};
        if (trimmed !== editing?.name) body.new_name = trimmed;
        if (savePath !== editing?.save_path) body.save_path = savePath;
        // An unchanged form would send an empty PATCH; just close.
        if (Object.keys(body).length === 0) {
          onClose(false);
          return;
        }
        const { error } = await api.PATCH("/categories/{name}", {
          params: { path: { name: state.category.name } },
          body,
        });
        if (error !== undefined) {
          report(error);
          return;
        }
      }
      onClose(true);
    } catch {
      toast.error(
        t("downloads.catSaveFailed", { detail: ct("shell.networkError") }),
      );
    } finally {
      setBusy(false);
    }
  };

  const report = (error: Problem | undefined) => {
    const detail = problemDetail(error) ?? ct("shell.networkError");
    if (isPathRejected(error)) {
      setPathError(detail);
      return;
    }
    toast.error(t("downloads.catSaveFailed", { detail }));
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose(false);
      }}
    >
      <DialogContent className="sm:max-w-md" aria-label={title}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="cat-name">{t("downloads.catDialog.name")}</Label>
            <Input
              id="cat-name"
              className="w-64"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </div>
          <PathField
            id="cat-save-path"
            label={t("downloads.catDialog.savePath")}
            value={savePath}
            error={pathError ?? undefined}
            browseText={t("downloads.browse")}
            browseLabel={t("downloads.catDialog.browseSavePath")}
            onBrowse={() => setBrowse(true)}
          />
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => onClose(false)}
          >
            {t("downloads.catDialog.cancel")}
          </Button>
          <Button
            disabled={busy || name.trim() === "" || savePath === ""}
            onClick={() => void submit()}
          >
            {t("downloads.catDialog.save")}
          </Button>
        </DialogFooter>
        <FolderBrowserDialog
          open={browse}
          initialPath={savePath}
          onSelect={(selected) => {
            setSavePath(selected);
            setBrowse(false);
            setPathError(null);
          }}
          onOpenChange={setBrowse}
        />
      </DialogContent>
    </Dialog>
  );
}

/** One confirm step before a DELETE — a watch folder's directory and a
 *  category's tasks are never touched, but the row itself is gone. */
function ConfirmDeleteDialog({
  target,
  onCancel,
  onConfirm,
}: {
  target: DeleteTarget | null;
  onCancel: () => void;
  onConfirm: () => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  // target clears as the dialog starts closing, but Radix keeps the content
  // mounted through the exit animation — render the body from the last
  // non-null target so the copy does not degrade mid-fade.
  const lastTargetRef = useRef<DeleteTarget | null>(null);
  if (target !== null) lastTargetRef.current = target;
  const shown = target ?? lastTargetRef.current;
  return (
    <Dialog
      open={target !== null}
      onOpenChange={(open) => {
        if (!open) onCancel();
      }}
    >
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("downloads.deleteTitle")}</DialogTitle>
          <DialogDescription className="text-sm">
            {shown?.kind === "watch"
              ? t("downloads.deleteWatchBody", { path: shown.row.path })
              : t("downloads.deleteCategoryBody", {
                  name: shown?.row.name ?? "",
                })}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel}>
            {t("downloads.deleteCancel")}
          </Button>
          <Button variant="destructive" onClick={onConfirm}>
            {t("downloads.deleteConfirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
