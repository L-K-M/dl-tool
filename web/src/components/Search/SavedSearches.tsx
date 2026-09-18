import { useState, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { Check, ChevronDown, Pencil, Trash2, X } from "lucide-react";

import { useUiPrefs, type SavedSearch } from "../../store/useUiPrefs";
import { Button } from "../ui/button";
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
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";

const MAX_SAVED = 50;
const MAX_NAME_LENGTH = 64;

/** Stable refusal codes; the dialog maps each to its inline reason. */
export function validateSavedName(
  name: string,
  existing: SavedSearch[],
): string | null {
  const trimmed = name.trim();
  if (trimmed === "") return "empty";
  if (trimmed.length > MAX_NAME_LENGTH) return "tooLong";
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
      ? t("search.savedErrDuplicate", {
          defaultValue: "A saved search with that name already exists.",
        })
      : code === "tooLong"
        ? t("search.savedErrTooLong", {
            defaultValue: "Keep the name under 64 characters.",
          })
        : code === "emptyQuery"
          ? t("search.savedErrNoQuery", {
              defaultValue: "Run a search first — there is nothing to save.",
            })
          : t("search.savedErrEmpty", { defaultValue: "Enter a name." });

  const submit = () => {
    if (saved.length >= MAX_SAVED) {
      setOpen(false);
      toast.error(
        t("search.savedCap", {
          defaultValue:
            "Saved searches are capped at {{count}} — delete one first.",
          count: MAX_SAVED,
        }),
      );
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
        id: crypto.randomUUID(),
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
    toast.success(
      t("search.savedOk", {
        defaultValue: "Saved {{name}}",
        name: name.trim(),
      }),
    );
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
      <Button size="sm" variant="outline" onClick={() => setOpen(true)}>
        {t("search.save", { defaultValue: "Save…" })}
      </Button>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>
            {t("search.savedDialogTitle", { defaultValue: "Save search" })}
          </DialogTitle>
          <DialogDescription>
            {t("search.savedDialogDesc", {
              defaultValue: "Name this search to re-run it later.",
            })}
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="saved-search-name">
            {t("search.savedNameLabel", { defaultValue: "Name" })}
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
            <p className="text-sm text-destructive">{reason(refusal)}</p>
          )}
        </div>
        <DialogFooter>
          <Button size="sm" onClick={submit}>
            {t("search.savedConfirm", { defaultValue: "Save" })}
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
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button size="sm" variant="outline">
          {t("search.savedMenu", { defaultValue: "Saved" })}{" "}
          <ChevronDown aria-hidden="true" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 gap-1 p-1.5">
        {saved.length === 0 ? (
          <p className="px-1.5 py-2 text-sm text-muted-foreground">
            {t("search.savedEmpty", { defaultValue: "No saved searches yet." })}
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
                        aria-label={t("search.savedRename", {
                          defaultValue: "Rename {{name}}",
                          name: s.name,
                        })}
                        autoFocus
                        onChange={(e) => {
                          setRenameText(e.target.value);
                          setRenameRefusal(null);
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") commitRename(s);
                          if (e.key === "Escape") setRenamingId(null);
                        }}
                      />
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={t("search.savedRenameConfirm", {
                          defaultValue: "Confirm rename",
                        })}
                        onClick={() => commitRename(s)}
                      >
                        <Check aria-hidden="true" />
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-label={t("search.savedRenameCancel", {
                          defaultValue: "Cancel rename",
                        })}
                        onClick={() => setRenamingId(null)}
                      >
                        <X aria-hidden="true" />
                      </Button>
                    </div>
                    {renameRefusal !== null && (
                      <p className="text-sm text-destructive">
                        {renameRefusal === "duplicate"
                          ? t("search.savedErrDuplicate", {
                              defaultValue:
                                "A saved search with that name already exists.",
                            })
                          : renameRefusal === "tooLong"
                            ? t("search.savedErrTooLong", {
                                defaultValue:
                                  "Keep the name under 64 characters.",
                              })
                            : t("search.savedErrEmpty", {
                                defaultValue: "Enter a name.",
                              })}
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
                          defaultValue: "{{count}} new since last view",
                          count: props.newSince?.get(s.id),
                        })}
                      >
                        +{props.newSince?.get(s.id)}
                      </span>
                    )}
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t("search.savedRename", {
                        defaultValue: "Rename {{name}}",
                        name: s.name,
                      })}
                      onClick={() => beginRename(s)}
                    >
                      <Pencil aria-hidden="true" />
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={t("search.savedDelete", {
                        defaultValue: "Delete {{name}}",
                        name: s.name,
                      })}
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
