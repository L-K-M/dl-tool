import { useState, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Check, ChevronDown, Pencil, Trash2, X } from "lucide-react";

import {
  MAX_SAVED_NAME_LENGTH,
  MAX_SAVED_SEARCHES,
  useUiPrefs,
  type SavedSearch,
} from "../../store/useUiPrefs";
import { Button } from "../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";

/** Stable refusal codes; the dialog maps each to its inline reason. */
export function validateSavedName(
  name: string,
  existing: SavedSearch[],
): string | null {
  const trimmed = name.trim();
  if (trimmed === "") return "empty";
  if (trimmed.length > MAX_SAVED_NAME_LENGTH) return "tooLong";
  if (existing.some((s) => s.name === trimmed)) return "duplicate";
  return null;
}

function patchSaved(next: SavedSearch[]): void {
  const current = useUiPrefs.getState().search;
  useUiPrefs.getState().patch({ search: { ...current, saved: next } });
}

/** The `Save…` control: captures the live query and both selections into the
 *  server-side preference document as a named saved search. */
export function SaveSearchButton(props: {
  query: string;
  indexerIds: string[];
  categories: number[];
}): JSX.Element {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);
  const saved = useUiPrefs((s) => s.search.saved);

  const reason = (code: string): string =>
    code === "duplicate"
      ? t("search.savedErrDuplicate")
      : code === "tooLong"
        ? t("search.savedErrTooLong")
        : code === "emptyQuery"
          ? t("search.savedErrNoQuery")
          : t("search.savedErrEmpty");

  const submit = () => {
    if (saved.length >= MAX_SAVED_SEARCHES) {
      setOpen(false);
      toast.error(t("search.savedCap", { count: MAX_SAVED_SEARCHES }));
      return;
    }
    const code = validateSavedName(name, saved);
    if (code !== null) {
      setRefusal(code);
      return;
    }
    const query = props.query.trim();
    if (query === "") {
      setRefusal("emptyQuery");
      return;
    }
    patchSaved([
      ...saved,
      {
        // crypto.randomUUID needs a secure context; a self-hosted instance
        // reached over plain HTTP on a LAN falls back to a timestamped id.
        id:
          typeof crypto.randomUUID === "function"
            ? crypto.randomUUID()
            : `sv_${Date.now().toString(36)}-${Math.random()
                .toString(36)
                .slice(2)}`,
        name: name.trim(),
        query,
        indexerIds: props.indexerIds,
        categories: props.categories,
        createdAt: new Date().toISOString(),
        lastTotal: 0,
      },
    ]);
    setOpen(false);
    setName("");
    setRefusal(null);
    toast.success(t("search.savedOk", { name: name.trim() }));
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) {
          setName("");
          setRefusal(null);
        }
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" type="button">
          {t("search.save")}
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{t("search.savedDialogTitle")}</DialogTitle>
          <DialogDescription>{t("search.savedDialogDesc")}</DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="saved-search-name">
            {t("search.savedNameLabel")}
          </Label>
          <Input
            id="saved-search-name"
            value={name}
            autoFocus
            onChange={(e) => {
              setName(e.target.value);
              setRefusal(null);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") submit();
            }}
          />
          {refusal !== null && (
            <p role="alert" className="text-sm text-destructive">
              {reason(refusal)}
            </p>
          )}
        </div>
        <DialogFooter>
          <Button size="sm" onClick={submit}>
            {t("search.savedConfirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** The `Saved ▾` popover: re-runs a saved search with exactly its stored
 *  selection, and renames or deletes entries in place. `newSince` carries the
 *  per-entry count of results a later run added over `lastTotal` — the "new
 *  since last view" badge (doc 09 section 7). */
export function SavedSearchesMenu(props: {
  onRun: (s: SavedSearch) => void;
  newSince?: ReadonlyMap<string, number>;
}): JSX.Element {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const saved = useUiPrefs((s) => s.search.saved);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [renameText, setRenameText] = useState("");
  const [renameRefusal, setRenameRefusal] = useState<string | null>(null);

  const beginRename = (s: SavedSearch) => {
    setRenamingId(s.id);
    setRenameText(s.name);
    setRenameRefusal(null);
  };

  const commitRename = (s: SavedSearch) => {
    const code = validateSavedName(
      renameText,
      saved.filter((e) => e.id !== s.id),
    );
    if (code !== null) {
      setRenameRefusal(code);
      return;
    }
    patchSaved(
      saved.map((e) => (e.id === s.id ? { ...e, name: renameText.trim() } : e)),
    );
    setRenamingId(null);
    setRenameRefusal(null);
  };

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        // A half-finished rename must not greet the next open.
        if (!next) setRenamingId(null);
      }}
    >
      <PopoverTrigger asChild>
        <Button size="sm" variant="outline" type="button">
          {t("search.savedMenu")} <ChevronDown aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 gap-1 p-1.5">
        {saved.length === 0 ? (
          <p className="px-1.5 py-2 text-sm text-muted-foreground">
            {t("search.savedEmpty")}
          </p>
        ) : (
          <ul className="max-h-64 overflow-auto">
            {saved.map((s) => (
              <li
                key={s.id}
                className="flex items-center gap-1 rounded px-1 py-0.5"
              >
                {renamingId === s.id ? (
                  <div className="flex min-w-0 flex-1 flex-col gap-1">
                    <div className="flex items-center gap-1">
                      <Input
                        value={renameText}
                        aria-label={t("search.savedRename", { name: s.name })}
                        autoFocus
                        onChange={(e) => {
                          setRenameText(e.target.value);
                          setRenameRefusal(null);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") commitRename(s);
                          if (e.key === "Escape") {
                            // Keep Radix's dismiss layer from closing the
                            // whole popover on a rename cancel.
                            e.stopPropagation();
                            setRenamingId(null);
                          }
                        }}
                      />
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={t("search.savedRenameConfirm")}
                        onClick={() => commitRename(s)}
                      >
                        <Check aria-hidden="true" />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={t("search.savedRenameCancel")}
                        onClick={() => setRenamingId(null)}
                      >
                        <X aria-hidden="true" />
                      </Button>
                    </div>
                    {renameRefusal !== null && (
                      <p role="alert" className="text-sm text-destructive">
                        {renameRefusal === "duplicate"
                          ? t("search.savedErrDuplicate")
                          : renameRefusal === "tooLong"
                            ? t("search.savedErrTooLong")
                            : t("search.savedErrEmpty")}
                      </p>
                    )}
                  </div>
                ) : (
                  <>
                    <button
                      type="button"
                      className="min-w-0 flex-1 truncate rounded px-1 text-left text-sm hover:bg-accent"
                      onClick={() => {
                        setOpen(false);
                        props.onRun(s);
                      }}
                    >
                      {s.name}
                    </button>
                    {(props.newSince?.get(s.id) ?? 0) > 0 && (
                      <span
                        className="rounded-full bg-accent px-1.5 text-xs tabular-nums"
                        title={t("search.savedNew", {
                          count: props.newSince?.get(s.id),
                        })}
                      >
                        +{props.newSince?.get(s.id)}
                      </span>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t("search.savedRename", { name: s.name })}
                      onClick={() => beginRename(s)}
                    >
                      <Pencil aria-hidden="true" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t("search.savedDelete", { name: s.name })}
                      onClick={() =>
                        patchSaved(saved.filter((e) => e.id !== s.id))
                      }
                    >
                      <Trash2 aria-hidden="true" />
                    </Button>
                  </>
                )}
              </li>
            ))}
          </ul>
        )}
      </PopoverContent>
    </Popover>
  );
}
