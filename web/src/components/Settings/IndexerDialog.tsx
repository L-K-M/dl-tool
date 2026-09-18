import type { TFunction } from "i18next";
import { useEffect, useRef, useState, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
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
import type { IndexerRow } from "./IndexersSection";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type Problem = components["schemas"]["ErrorModel"];
type IndexerDTO = components["schemas"]["IndexerDTO"];
type CreateBody = components["schemas"]["CreateIndexerInputBody"];
type PatchBody = components["schemas"]["PatchIndexerInputBody"];

export type IndexerDialogState =
  { mode: "add" } | { mode: "edit"; indexer: IndexerRow } | { mode: "import" };

const selectClass =
  "h-8 w-full rounded-lg border border-input bg-transparent px-2 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 outline-none";

function problemDetail(error: Problem | undefined): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

/** The documented error mapping of doc 05 §9.1, shared by the add/edit and
 *  import forms: 409 conflict, 403 /problems/ssrf-blocked naming the blocked
 *  target, 422 naming the field in errors[].location, else the fallback. */
function reportIndexerError(
  t: TFunction,
  ct: TFunction,
  error: Problem | undefined,
  opts: {
    blocked: string;
    blockedFile?: string;
    fallback: "saveFailed" | "importFailed";
  },
): void {
  const detail = problemDetail(error) ?? ct("shell.networkError");
  if (error?.status === 409 || error?.type === "/problems/conflict") {
    toast.error(t("indexers.dialog.conflict"));
  } else if (error?.type === "/problems/ssrf-blocked") {
    toast.error(
      opts.blockedFile === undefined
        ? t("indexers.dialog.ssrfBlocked", { url: opts.blocked, detail })
        : t("indexers.dialog.ssrfBlockedFile", {
            file: opts.blockedFile,
            detail,
          }),
    );
  } else if (error?.status === 422) {
    const field = error.errors?.[0]?.location;
    toast.error(
      field === undefined
        ? t("indexers.dialog.invalidFieldGeneric", { detail })
        : t("indexers.dialog.invalidField", { field, detail }),
    );
  } else {
    toast.error(t(`indexers.dialog.${opts.fallback}`, { detail }));
  }
}

/** onClose(true) means the caller must refetch GET /indexers. */
export function IndexerDialog({
  state,
  onClose,
}: {
  state: IndexerDialogState | null;
  onClose: (changed: boolean) => void;
}): JSX.Element | null {
  const { t } = useTranslation("settings");
  // Tracks a completed write whose result stays on screen (the import
  // warnings), so an Esc or overlay close still tells the caller to refetch.
  const changed = useRef(false);
  useEffect(() => {
    changed.current = false;
  }, [state]);

  if (state === null) return null;

  const finish = (didChange: boolean) => {
    changed.current = false;
    onClose(didChange);
  };
  const markChanged = () => {
    changed.current = true;
  };

  const title =
    state.mode === "add"
      ? t("indexers.dialog.addTitle")
      : state.mode === "edit"
        ? t("indexers.dialog.editTitle")
        : t("indexers.dialog.importTitle");

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) finish(changed.current);
      }}
    >
      <DialogContent className="sm:max-w-md" aria-label={title}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        {state.mode === "import" ? (
          <ImportForm finish={finish} markChanged={markChanged} />
        ) : (
          <EditForm
            key={state.mode === "edit" ? state.indexer.id : "add"}
            state={state}
            finish={finish}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function EditForm({
  state,
  finish,
}: {
  state: { mode: "add" } | { mode: "edit"; indexer: IndexerRow };
  finish: (changed: boolean) => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const editing = state.mode === "edit" ? state.indexer : null;
  const [name, setName] = useState(editing?.name ?? "");
  const [kind, setKind] = useState<IndexerRow["kind"]>(
    editing?.kind ?? "torznab",
  );
  const [url, setUrl] = useState(editing?.url ?? "");
  // Always starts empty: the stored key is never returned, so it is sent only
  // when the user types a new one.
  const [apiKey, setApiKey] = useState("");
  const [enabled, setEnabled] = useState(editing?.enabled ?? true);
  const [priority, setPriority] = useState(String(editing?.priority ?? 50));
  const [busy, setBusy] = useState(false);

  const report = (error: Problem | undefined) =>
    reportIndexerError(t, ct, error, {
      blocked: url.trim(),
      fallback: "saveFailed",
    });

  const submit = async () => {
    const parsed = Number.parseInt(priority, 10);
    if (!Number.isFinite(parsed)) {
      toast.error(
        t("indexers.dialog.invalidField", {
          field: "priority",
          detail: priority,
        }),
      );
      return;
    }
    setBusy(true);
    try {
      if (state.mode === "add") {
        const body: CreateBody = {
          name,
          kind,
          enabled,
          priority: parsed,
        };
        if (url.trim() !== "") body.url = url.trim();
        if (apiKey !== "") body.api_key = apiKey;
        const { error } = await api.POST("/indexers", { body });
        if (error !== undefined) {
          report(error);
          return;
        }
      } else {
        const body: PatchBody = {
          name,
          kind,
          enabled,
          priority: parsed,
        };
        // Send the URL whenever it changed — including a cleared field, so a
        // server-side 422 names url instead of silently keeping the old value.
        if (url.trim() !== (editing?.url ?? "")) body.url = url.trim();
        if (apiKey !== "") body.api_key = apiKey;
        const { error } = await api.PATCH("/indexers/{id}", {
          params: { path: { id: state.indexer.id } },
          body,
        });
        if (error !== undefined) {
          report(error);
          return;
        }
      }
      finish(true);
    } catch {
      toast.error(
        t("indexers.dialog.saveFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <div className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-name">{t("indexers.dialog.name")}</Label>
          <Input
            id="indexer-name"
            className="w-64"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-kind">{t("indexers.dialog.kind")}</Label>
          <select
            id="indexer-kind"
            className={selectClass + " w-64"}
            value={kind}
            onChange={(event) =>
              setKind(event.target.value as IndexerRow["kind"])
            }
          >
            <option value="torznab">{t("indexers.dialog.kindTorznab")}</option>
            <option value="newznab">{t("indexers.dialog.kindNewznab")}</option>
            <option value="dlsearch">
              {t("indexers.dialog.kindDlsearch")}
            </option>
          </select>
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-url">{t("indexers.dialog.url")}</Label>
          <Input
            id="indexer-url"
            className="w-64"
            value={url}
            onChange={(event) => setUrl(event.target.value)}
          />
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-key">{t("indexers.dialog.apiKey")}</Label>
          <Input
            id="indexer-key"
            type="password"
            className="w-64"
            autoComplete="off"
            value={apiKey}
            onChange={(event) => setApiKey(event.target.value)}
          />
        </div>
        {state.mode === "edit" && (
          <p className="text-xs text-muted-foreground">
            {t("indexers.dialog.apiKeyEditHint")}
          </p>
        )}
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-enabled">
            {t("indexers.dialog.enabled")}
          </Label>
          <Checkbox
            id="indexer-enabled"
            checked={enabled}
            onCheckedChange={(checked) => setEnabled(checked === true)}
          />
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-priority">
            {t("indexers.dialog.priority")}
          </Label>
          <Input
            id="indexer-priority"
            type="number"
            className="w-64"
            value={priority}
            onChange={(event) => setPriority(event.target.value)}
          />
        </div>
      </div>
      <DialogFooter>
        <Button variant="outline" disabled={busy} onClick={() => finish(false)}>
          {t("indexers.dialog.cancel")}
        </Button>
        <Button
          disabled={busy || name.trim() === ""}
          onClick={() => void submit()}
        >
          {state.mode === "add" ? t("indexers.add") : t("indexers.dialog.save")}
        </Button>
      </DialogFooter>
    </>
  );
}

function ImportForm({
  finish,
  markChanged,
}: {
  finish: (changed: boolean) => void;
  markChanged: () => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const [file, setFile] = useState<File | null>(null);
  const [torznabUrl, setTorznabUrl] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<{
    indexer: IndexerDTO;
    warnings: string[];
  } | null>(null);

  const report = (error: Problem | undefined) =>
    reportIndexerError(t, ct, error, {
      // A file import's blocked target is inside the file, not the URL field.
      blocked: torznabUrl.trim(),
      blockedFile: file?.name,
      fallback: "importFailed",
    });

  const submit = async () => {
    if (file === null && (torznabUrl.trim() === "" || apiKey === "")) {
      toast.error(t("indexers.dialog.importNeedsInput"));
      return;
    }
    setBusy(true);
    try {
      const { data, error } =
        file !== null
          ? await api.POST("/indexers/import", {
              body: { file: file.name },
              bodySerializer: () => {
                const form = new FormData();
                form.set("file", file, file.name);
                return form;
              },
            })
          : await api.POST("/indexers/import", {
              body: { torznab_url: torznabUrl.trim(), api_key: apiKey },
            });
      if (data === undefined) {
        report(error);
        return;
      }
      markChanged();
      setOutcome({
        indexer: data.indexer,
        warnings: data.warnings ?? [],
      });
    } catch {
      toast.error(
        t("indexers.dialog.importFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setBusy(false);
    }
  };

  if (outcome !== null) {
    return (
      <>
        <div className="flex flex-col gap-2 text-sm">
          <p>{t("indexers.dialog.imported", { name: outcome.indexer.name })}</p>
          {outcome.indexer.provenance !== null && (
            <p className="text-xs text-muted-foreground">
              {outcome.indexer.provenance}
            </p>
          )}
          {outcome.warnings.length > 0 && (
            <div>
              <p className="font-medium">{t("indexers.dialog.warnings")}</p>
              <ul className="list-disc ps-5 text-muted-foreground">
                {outcome.warnings.map((warning) => (
                  <li key={warning}>{warning}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
        <DialogFooter>
          <Button onClick={() => finish(true)}>
            {t("indexers.dialog.done")}
          </Button>
        </DialogFooter>
      </>
    );
  }

  return (
    <>
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <Label htmlFor="indexer-file">{t("indexers.dialog.file")}</Label>
          <Input
            id="indexer-file"
            type="file"
            accept=".dlsearch.yaml,.dlm,.py"
            onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          />
          <p className="text-xs text-muted-foreground">
            {t("indexers.dialog.fileHint")}
          </p>
        </div>
        <p className="text-xs text-muted-foreground">
          {t("indexers.dialog.importOr")}
        </p>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-torznab-url">
            {t("indexers.dialog.torznabUrl")}
          </Label>
          <Input
            id="indexer-torznab-url"
            className="w-64"
            value={torznabUrl}
            onChange={(event) => setTorznabUrl(event.target.value)}
          />
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="indexer-import-key">
            {t("indexers.dialog.apiKey")}
          </Label>
          <Input
            id="indexer-import-key"
            type="password"
            className="w-64"
            autoComplete="off"
            value={apiKey}
            onChange={(event) => setApiKey(event.target.value)}
          />
        </div>
      </div>
      <DialogFooter>
        <Button variant="outline" disabled={busy} onClick={() => finish(false)}>
          {t("indexers.dialog.cancel")}
        </Button>
        <Button disabled={busy} onClick={() => void submit()}>
          {t("indexers.dialog.submit")}
        </Button>
      </DialogFooter>
    </>
  );
}
