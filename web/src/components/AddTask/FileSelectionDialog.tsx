import { useEffect, useMemo, useState, type JSX } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ChevronLeft, ChevronRight, X } from "lucide-react";
import { api } from "../../api/client";
import { initI18n } from "../../i18n";
import strings from "../../locales/en/dialogs.json";
import {
  buildTree,
  FileTree,
  type FileChange,
  type FilePriority,
} from "../FileTree/FileTree";
import type { AddTaskDraft } from "./AddTaskDialog";
import { FolderBrowserDialog } from "../FolderBrowser/FolderBrowserDialog";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import {
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";

initI18n().addResourceBundle("en", "dialogs", strings);

export interface Manifest {
  source_uri: string;
  kind: string;
  name: string;
  total_size: number | null;
  file_count: number | null;
  metadata_pending: boolean;
  infohash_v1: string | null;
  infohash_v2: string | null;
  files: { index: number; path: string; size: number | null }[] | null;
}

export interface SelectionEntry {
  selected: boolean;
  priority: FilePriority;
}

/** The onCreate selection row of the interface contract — priority travels as
 *  the plain string the create body narrows to its enum. */
export interface SelectionRow {
  index: number;
  selected: boolean;
  priority: string;
}

const filterDebounceMs = 250;
const sizeUnits: Record<string, number> = {
  b: 1,
  kb: 1024,
  mb: 1024 ** 2,
  gb: 1024 ** 3,
  tb: 1024 ** 4,
};

interface ManifestFile {
  index: number;
  path: string;
  size: number | null;
}

// The same token language FileTree applies to its rows (doc 09 §5): the
// All / None / Invert buttons act on exactly the set the filter leaves
// visible, so it is computed here over the flat manifest list.
function tokenMatches(file: ManifestFile, token: string): boolean {
  if (token.startsWith("ext:")) {
    const ext = token.slice(4);
    return ext !== "" && file.path.toLowerCase().endsWith(`.${ext}`);
  }
  const size = /^>(\d+(?:\.\d+)?)\s*(b|kb|mb|gb|tb)?$/.exec(token);
  if (size) {
    const unit = sizeUnits[(size[2] ?? "b").toLowerCase()] ?? 1;
    return (file.size ?? 0) > Number(size[1]) * unit;
  }
  return file.path.toLowerCase().includes(token);
}

function matchingIndices(files: ManifestFile[], filter: string): number[] {
  const tokens = filter
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .filter((token) => token !== "");
  return files
    .filter((file) => tokens.every((token) => tokenMatches(file, token)))
    .map((file) => file.index);
}

/** The file-selection step of doc 09 §5, rendered inside the add dialog's own
 *  modal as its second page. It owns per-page selection state; onCreate hands
 *  the first multi-file manifest's selection to the caller, the only manifest
 *  select_files can describe (doc 05 §5.2). */
export function FileSelectionDialog({
  manifests,
  draft,
  onBack,
  onCreate,
}: {
  manifests: Manifest[];
  draft: AddTaskDraft;
  onBack: () => void;
  onCreate: (
    draft: AddTaskDraft,
    selection: { index: number; selected: boolean; priority: string }[],
  ) => void;
}): JSX.Element {
  const { t } = useTranslation("dialogs");
  const [page, setPage] = useState(0);
  const [filterInput, setFilterInput] = useState("");
  const [filter, setFilter] = useState("");
  // The wireframe keeps destination and the subfolder option editable on this
  // step; both travel back inside the draft onCreate receives.
  const [destination, setDestination] = useState(draft.destination);
  const [subfolder, setSubfolder] = useState(draft.create_subfolder);
  const [browseOpen, setBrowseOpen] = useState(false);
  // FileTree owns its collapsed set internally; remounting expands every
  // folder at once — the only expand-all the component exposes.
  const [treeKey, setTreeKey] = useState(0);
  const [pages, setPages] = useState<Map<number, Map<number, SelectionEntry>>>(
    new Map(),
  );

  useEffect(() => {
    const timer = setTimeout(() => setFilter(filterInput), filterDebounceMs);
    return () => clearTimeout(timer);
  }, [filterInput]);

  const manifest = manifests[Math.min(page, manifests.length - 1)];
  const files = useMemo(() => manifest?.files ?? [], [manifest]);
  const entries = pages.get(page) ?? new Map<number, SelectionEntry>();
  const entryFor = (index: number): SelectionEntry =>
    entries.get(index) ?? { selected: true, priority: "normal" };

  const nodes = useMemo(
    () =>
      buildTree(
        files.map((file) => ({
          index: file.index,
          path: file.path,
          size_bytes: file.size,
          selected: entryFor(file.index).selected,
          priority: entryFor(file.index).priority,
        })),
      ),
    [files, entries],
  );

  const freeQuery = useQuery({
    queryKey: ["fs-free-space", destination],
    enabled: destination !== "",
    queryFn: async () => {
      const { data } = await api.GET("/fs/free-space", {
        params: { query: { path: destination } },
      });
      return data ?? null;
    },
  });
  const freeBytes = freeQuery.data?.free_bytes ?? null;

  // The shortfall counts the whole submission, not just the visible page:
  // unvisited manifests download at their defaults (doc 09 §5).
  const wantedBytes = manifests.reduce(
    (sum, m, manifestIndex) =>
      sum +
      (m.files ?? []).reduce((inner, file) => {
        const entry = pages.get(manifestIndex)?.get(file.index) ?? {
          selected: true,
        };
        return inner + (entry.selected ? (file.size ?? 0) : 0);
      }, 0),
    0,
  );
  const shortfall = freeBytes !== null && wantedBytes > freeBytes;

  const applyChanges = (changes: FileChange[]) => {
    setPages((previous) => {
      const next = new Map(previous);
      const state = new Map(next.get(page) ?? []);
      for (const change of changes) {
        const current = state.get(change.index) ?? {
          selected: true,
          priority: "normal",
        };
        // Doc 09 §5: the checkbox and the priority are one concept — Skip
        // unchecks the row, unchecking sets Skip and they never disagree.
        let entry = current;
        if (change.priority === "skip" || change.selected === false)
          entry = { selected: false, priority: "skip" };
        else if (change.priority !== undefined)
          entry = { selected: true, priority: change.priority };
        else if (change.selected === true)
          entry = {
            selected: true,
            priority: current.priority === "skip" ? "normal" : current.priority,
          };
        state.set(change.index, entry);
      }
      next.set(page, state);
      return next;
    });
  };

  const setFiltered = (selected: boolean | "invert") => {
    applyChanges(
      matchingIndices(files, filter).map((index) => ({
        index,
        selected: selected === "invert" ? !entryFor(index).selected : selected,
      })),
    );
  };

  /** The explicit selection of the first multi-file manifest; unvisited
   *  manifests keep their defaults (doc 09 §5). */
  const selection = (): {
    index: number;
    selected: boolean;
    priority: string;
  }[] => {
    const first = manifests.findIndex((m) => (m.files?.length ?? 0) > 1);
    if (first < 0) return [];
    const state = pages.get(first) ?? new Map<number, SelectionEntry>();
    return (manifests[first].files ?? []).map((file) => ({
      index: file.index,
      selected: state.get(file.index)?.selected ?? true,
      priority: state.get(file.index)?.priority ?? "normal",
    }));
  };

  const filteredSuffix = filter.trim() !== "";
  const multi = files.length > 1;
  const outDraft: AddTaskDraft = {
    ...draft,
    destination,
    create_subfolder: subfolder,
  };

  return (
    <>
      <DialogHeader>
        <DialogTitle>
          {t("addTask.select.title", { name: manifest?.name ?? "" })}
        </DialogTitle>
        <DialogDescription>{manifest?.source_uri}</DialogDescription>
      </DialogHeader>

      <div className="flex items-center gap-2">
        <Input
          readOnly
          aria-label={t("addTask.destination")}
          value={destination}
          placeholder={t("addTask.destinationDefault")}
          className="min-w-0 flex-1"
        />
        <Button variant="outline" onClick={() => setBrowseOpen(true)}>
          {t("addTask.selectDestination")}
        </Button>
        <label
          className="flex shrink-0 items-center gap-2 text-sm"
          title={t("addTask.subfolderTip")}
        >
          <Checkbox
            checked={subfolder}
            onCheckedChange={(checked) => setSubfolder(checked === true)}
          />
          {t("addTask.subfolder")}
        </label>
      </div>

      {manifest?.metadata_pending ? (
        // Doc 05 §5.3: metadata did not arrive inside the deadline, so the
        // manifest carries no file list and the step's Cancel creates the
        // task paused instead (doc 09 §5).
        <p className="py-4">{t("addTask.select.fetching")}</p>
      ) : (
        <>
          <div className="flex items-center gap-2">
            <span className="relative min-w-0 flex-1">
              <Input
                aria-label={t("addTask.select.filter")}
                placeholder={t("addTask.select.filter")}
                value={filterInput}
                onChange={(event) => setFilterInput(event.target.value)}
              />
              {filterInput !== "" ? (
                <button
                  type="button"
                  aria-label={t("addTask.select.clearFilter")}
                  className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground"
                  onClick={() => setFilterInput("")}
                >
                  <X className="size-3.5" aria-hidden="true" />
                </button>
              ) : null}
            </span>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setFiltered(true)}
            >
              {filteredSuffix
                ? t("addTask.select.allFiltered")
                : t("addTask.select.all")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setFiltered(false)}
            >
              {filteredSuffix
                ? t("addTask.select.noneFiltered")
                : t("addTask.select.none")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setFiltered("invert")}
            >
              {filteredSuffix
                ? t("addTask.select.invertFiltered")
                : t("addTask.select.invert")}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setTreeKey((key) => key + 1)}
            >
              {t("addTask.select.expandAll")}
            </Button>
          </div>
          <FileTree
            key={`${page}-${treeKey}`}
            nodes={nodes}
            readOnly={!multi}
            filter={filter}
            freeBytes={freeBytes}
            onChange={applyChanges}
          />
        </>
      )}

      <DialogFooter>
        <span className="mr-auto flex items-center gap-1">
          {manifests.length > 1 ? (
            <>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={t("addTask.select.prev")}
                disabled={page === 0}
                onClick={() => setPage((value) => Math.max(0, value - 1))}
              >
                <ChevronLeft aria-hidden="true" />
              </Button>
              <span className="text-sm tabular-nums">
                {t("addTask.select.pager", {
                  index: page + 1,
                  total: manifests.length,
                })}
              </span>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={t("addTask.select.next")}
                disabled={page >= manifests.length - 1}
                onClick={() =>
                  setPage((value) => Math.min(manifests.length - 1, value + 1))
                }
              >
                <ChevronRight aria-hidden="true" />
              </Button>
            </>
          ) : null}
        </span>
        <Button variant="outline" onClick={onBack}>
          {t("addTask.select.back")}
        </Button>
        {manifest?.metadata_pending ? (
          <Button
            variant="outline"
            onClick={() => onCreate({ ...outDraft, paused: true }, [])}
          >
            {t("addTask.select.cancel")}
          </Button>
        ) : (
          <DialogClose asChild>
            <Button variant="outline">{t("addTask.select.cancel")}</Button>
          </DialogClose>
        )}
        {manifest?.metadata_pending ? null : (
          <Button
            disabled={shortfall}
            title={shortfall ? t("addTask.select.shortfall") : undefined}
            onClick={() => onCreate(outDraft, selection())}
          >
            {t("addTask.select.create")}
          </Button>
        )}
      </DialogFooter>
      <FolderBrowserDialog
        open={browseOpen}
        initialPath={destination !== "" ? destination : undefined}
        onOpenChange={setBrowseOpen}
        onSelect={(path) => setDestination(path)}
      />
    </>
  );
}
