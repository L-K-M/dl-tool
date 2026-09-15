import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type KeyboardEvent,
} from "react";
import { useTranslation } from "react-i18next";
import {
  CornerLeftUpIcon,
  FolderIcon,
  FolderPlusIcon,
  LockIcon,
} from "lucide-react";

import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatBytes } from "../../lib/format";
import strings from "../../locales/en/dialogs.json";
import { Button } from "../ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

initI18n().addResourceBundle("en", "dialogs", strings);

type DirListing = components["schemas"]["Listing"];
type Fault = "pathRejected" | "notFound" | "nameConflict" | "mkdirFailed";

// One row of the listing: the ".." parent row first when the server sent
// a parent, then the directories. The parent row is navigation only —
// it is never a selection target.
interface Row {
  name: string;
  path: string | null;
  writable: boolean;
  isParent: boolean;
}

function rowsOf(listing: DirListing | null): Row[] {
  if (!listing) return [];
  const rows: Row[] = [];
  if (listing.parent !== null) {
    rows.push({
      name: "..",
      path: listing.parent,
      writable: true,
      isParent: true,
    });
  }
  for (const dir of listing.directories ?? []) {
    rows.push({
      name: dir.name,
      path: dir.path,
      writable: dir.writable,
      isParent: false,
    });
  }
  return rows;
}

export interface FolderBrowserDialogProps {
  open: boolean;
  /** Where to open. Falls back to the first entry of GET /fs/roots. */
  initialPath?: string;
  onSelect: (path: string) => void;
  onOpenChange: (open: boolean) => void;
}

/** The §4.1 server-side folder browser: a nested dialog that browses the
 *  jailed filesystem, creates folders and refuses unwritable directories.
 *  It returns a path through onSelect and nothing else — it never writes
 *  a destination anywhere itself. */
export function FolderBrowserDialog({
  open,
  initialPath,
  onSelect,
  onOpenChange,
}: FolderBrowserDialogProps) {
  const { t, i18n } = useTranslation("dialogs");
  const locale = i18n.language;

  const [listing, setListing] = useState<DirListing | null>(null);
  const [fault, setFault] = useState<Fault | null>(null);
  const [cursor, setCursor] = useState(-1);
  const [pathDraft, setPathDraft] = useState("");
  const [space, setSpace] = useState<{ free: number; total: number } | null>(
    null,
  );
  const [naming, setNaming] = useState(false);
  const [newName, setNewName] = useState("");

  const rows = useMemo(() => rowsOf(listing), [listing]);
  const cursorRow = cursor >= 0 && cursor < rows.length ? rows[cursor] : null;
  // The prospective selection: the highlighted directory row, else the
  // directory being browsed. The ".." row highlights but never selects.
  const highlighted = cursorRow && !cursorRow.isParent ? cursorRow : null;
  const targetWritable = highlighted
    ? highlighted.writable
    : (listing?.writable ?? false);
  const targetPath = highlighted?.path ?? listing?.path ?? null;

  // GET /fs/browse for one path. A rejection keeps the previous listing
  // on screen and only swaps the error line (doc 09 section 4.1).
  const navigate = useCallback(async (path: string): Promise<boolean> => {
    try {
      const { data, response } = await api.GET("/fs/browse", {
        params: { query: { path } },
      });
      if (!data) {
        setFault(response.status === 403 ? "pathRejected" : "notFound");
        return false;
      }
      setListing(data);
      setFault(null);
      setCursor(-1);
      setPathDraft(data.path);
      setSpace({ free: data.free_bytes, total: data.total_bytes });
      setNaming(false);
      return true;
    } catch {
      setFault("notFound");
      return false;
    }
  }, []);

  // Open: browse initialPath, or the first configured root when none was
  // given (doc 09 section 4.1 — the browser opens at the roots).
  useEffect(() => {
    if (!open) return;
    setListing(null);
    setFault(null);
    setCursor(-1);
    setSpace(null);
    setNaming(false);
    setNewName("");
    setPathDraft(initialPath ?? "");
    if (initialPath) {
      void navigate(initialPath);
      return;
    }
    void (async () => {
      try {
        const { data } = await api.GET("/fs/roots");
        const first = data?.roots?.[0];
        if (!first) {
          setFault("notFound");
          return;
        }
        void navigate(first.path);
      } catch {
        setFault("notFound");
      }
    })();
    // navigate is stable; initialPath is read once per opening — the open
    // transition always reloads.
  }, [open, navigate]);

  // GET /fs/free-space for the highlighted directory — the free-space
  // line follows the prospective selection. The current directory's own
  // figures already arrived inside the browse answer, so no second call.
  useEffect(() => {
    const path = highlighted?.path;
    if (!path) return;
    let stale = false;
    void (async () => {
      try {
        const { data } = await api.GET("/fs/free-space", {
          params: { query: { path } },
        });
        if (!stale && data) {
          setSpace({ free: data.free_bytes, total: data.total_bytes });
        }
      } catch {
        // Space is informational; a failed probe leaves the browse figure.
      }
    })();
    return () => {
      stale = true;
    };
  }, [highlighted?.path]);

  // Highlight a row, or fall back to the listing itself when the cursor
  // leaves the directories — the space line reverts to the browse figure.
  useEffect(() => {
    if (!highlighted && listing) {
      setSpace({ free: listing.free_bytes, total: listing.total_bytes });
    }
  }, [highlighted, listing]);

  function descend(row: Row | null) {
    if (!row) return;
    void navigate(row.path ?? "");
  }

  function ascend() {
    if (listing?.parent) void navigate(listing.parent);
  }

  function onListKeyDown(event: KeyboardEvent<HTMLDivElement>) {
    switch (event.key) {
      case "ArrowDown":
        setCursor((c) => Math.min(c + 1, rows.length - 1));
        event.preventDefault();
        break;
      case "ArrowUp":
        setCursor((c) => Math.max(c - 1, 0));
        event.preventDefault();
        break;
      case "Enter":
      case "ArrowRight":
        descend(cursorRow);
        event.preventDefault();
        break;
      case "Backspace":
      case "ArrowLeft":
        ascend();
        event.preventDefault();
        break;
    }
  }

  // The manual Path field validates on blur (and Enter) through the same
  // browse endpoint; a rejected path keeps the previous listing and the
  // field falls back to it.
  function commitPathDraft() {
    const path = pathDraft.trim();
    if (!path || path === listing?.path) {
      setPathDraft(listing?.path ?? pathDraft);
      return;
    }
    void navigate(path).then((ok) => {
      if (!ok) setPathDraft(listing?.path ?? "");
    });
  }

  async function createFolder() {
    const name = newName.trim();
    if (!listing || !name) return;
    try {
      const { data, response } = await api.POST("/fs/mkdir", {
        body: { path: listing.path, name },
      });
      if (data) {
        setNaming(false);
        setNewName("");
        // Re-list so the new directory appears in place, sorted.
        void navigate(listing.path);
        return;
      }
      if (response.status === 409) setFault("nameConflict");
      else setFault(response.status === 403 ? "pathRejected" : "mkdirFailed");
    } catch {
      setFault("mkdirFailed");
    }
  }

  const breadcrumb = useMemo(() => {
    if (!listing) return [];
    const segments = listing.path.split("/").filter((s) => s !== "");
    const crumbs: { name: string; path: string }[] = [{ name: "/", path: "/" }];
    let prefix = "";
    for (const segment of segments) {
      prefix += "/" + segment;
      crumbs.push({ name: segment, path: prefix });
    }
    return crumbs;
  }, [listing]);

  const selectTitle = targetWritable
    ? undefined
    : t("folderBrowser.notWritable");

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="sm:max-w-lg"
        aria-label={t("folderBrowser.title")}
      >
        <DialogHeader>
          <DialogTitle>{t("folderBrowser.title")}</DialogTitle>
        </DialogHeader>

        <nav
          aria-label={t("folderBrowser.title")}
          className="flex flex-wrap items-center gap-1 text-sm"
        >
          {breadcrumb.map((crumb, index) => (
            <span key={crumb.path} className="flex items-center gap-1">
              {index > 0 && <span aria-hidden="true">›</span>}
              <button
                type="button"
                className="rounded px-1 hover:bg-muted focus-visible:ring-3 focus-visible:ring-ring/50"
                onClick={() => void navigate(crumb.path)}
              >
                {crumb.name}
              </button>
            </span>
          ))}
        </nav>

        <div
          role="listbox"
          aria-label={t("folderBrowser.listLabel")}
          aria-activedescendant={
            cursor >= 0 ? `folder-row-${cursor}` : undefined
          }
          tabIndex={0}
          onKeyDown={onListKeyDown}
          className="max-h-64 min-h-40 overflow-y-auto rounded-lg border border-input outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        >
          {rows.map((row, index) => (
            <div
              key={row.isParent ? ".." : row.path}
              id={`folder-row-${index}`}
              role="option"
              aria-selected={index === cursor}
              className={`flex items-center gap-2 px-2 py-1 text-sm ${
                index === cursor ? "bg-muted" : ""
              }`}
              onClick={() => (row.isParent ? ascend() : setCursor(index))}
              onDoubleClick={() => !row.isParent && descend(row)}
            >
              {row.isParent ? (
                <CornerLeftUpIcon className="size-4 shrink-0" aria-hidden />
              ) : (
                <FolderIcon className="size-4 shrink-0" aria-hidden />
              )}
              <span className="truncate" title={row.name}>
                {row.name}
              </span>
              {!row.isParent && !row.writable && (
                <LockIcon
                  className="ml-auto size-3.5 shrink-0 text-muted-foreground"
                  role="img"
                  aria-label={t("folderBrowser.notWritable")}
                />
              )}
            </div>
          ))}
        </div>

        <div className="flex items-center gap-2">
          <Label htmlFor="folder-browser-path">
            {t("folderBrowser.pathLabel")}
          </Label>
          <Input
            id="folder-browser-path"
            value={pathDraft}
            onChange={(event) => setPathDraft(event.target.value)}
            onBlur={commitPathDraft}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                event.preventDefault();
                commitPathDraft();
              }
            }}
          />
        </div>

        {fault && (
          <p role="alert" className="text-sm text-destructive">
            {t(`folderBrowser.${fault}`)}
          </p>
        )}

        <div className="flex items-center justify-between gap-2">
          <span className="text-sm text-muted-foreground tabular-nums">
            {space
              ? t("folderBrowser.freeSpace", {
                  free: formatBytes(space.free, locale),
                  total: formatBytes(space.total, locale),
                })
              : null}
          </span>
          {naming ? (
            <span className="flex items-center gap-2">
              <Input
                aria-label={t("folderBrowser.newFolderName")}
                value={newName}
                autoFocus
                onChange={(event) => setNewName(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    event.preventDefault();
                    void createFolder();
                  }
                  if (event.key === "Escape") {
                    // Cancel the naming row without closing the dialog.
                    event.stopPropagation();
                    setNaming(false);
                    setNewName("");
                  }
                }}
              />
              <Button size="sm" onClick={() => void createFolder()}>
                {t("folderBrowser.createFolder")}
              </Button>
            </span>
          ) : (
            <Button
              variant="outline"
              size="sm"
              onClick={() => setNaming(true)}
              disabled={!listing?.writable}
              title={
                listing && !listing.writable
                  ? t("folderBrowser.notWritable")
                  : undefined
              }
            >
              <FolderPlusIcon aria-hidden />
              {t("folderBrowser.newFolder")}
            </Button>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("folderBrowser.cancel")}
          </Button>
          <Button
            disabled={!targetPath || !targetWritable}
            title={selectTitle}
            onClick={() => {
              if (targetPath) {
                onSelect(targetPath);
                onOpenChange(false);
              }
            }}
          >
            {t("folderBrowser.select")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
