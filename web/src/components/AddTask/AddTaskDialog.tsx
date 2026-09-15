import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent as ReactDragEvent,
  type JSX,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import {
  ChevronDown,
  ChevronRight,
  Eye,
  EyeOff,
  Upload,
  X,
} from "lucide-react";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatBytes } from "../../lib/format";
import strings from "../../locales/en/dialogs.json";
import { useTasks, type Task } from "../../store/useTasks";
import { useUiPrefs } from "../../store/useUiPrefs";
import { FolderBrowserDialog } from "../FolderBrowser/FolderBrowserDialog";
import {
  FileSelectionDialog,
  type Manifest,
  type SelectionRow,
} from "./FileSelectionDialog";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../ui/select";

initI18n().addResourceBundle("en", "dialogs", strings);

export interface AddTaskDraft {
  uris: string[];
  files: File[]; // .torrent and .metalink parts; a .txt is expanded client-side
  destination: string;
  category: string | null;
  tags: string[];
  paused: boolean;
  sequential: boolean;
  create_subfolder: boolean;
  ftp_credentials: { username: string; password: string } | null;
  extract_password: string | null;
  dl_limit: number; // bytes per second, 0 = unlimited
  ul_limit: number;
}

/** Per-line badge of doc 09 §4: a recognised scheme, an unrecognised one, or a duplicate of a task
 *  already in the store, matched on normalised URI or infohash. */
export type LineBadge = "ok" | "unknown" | "duplicate";

type CreateBody = components["schemas"]["CreateTasksBody"];
type InspectBody = components["schemas"]["InspectTasksBody"];
type FileSelectionRequest = components["schemas"]["FileSelectionRequest"];
type TaskDTO = components["schemas"]["TaskDTO"];
type Problem = components["schemas"]["ErrorModel"];

const MAX_URIS = 50;
const uriCap = MAX_URIS;
const recognisedScheme = /^(https?|ftps?|sftp|magnet):/i;
const hexInfohash = /^[0-9a-f]{40}$/i;
const base32Infohash = /^[2-7a-z]{32}$/i;
const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

/** qBittorrent's own drop predicate, doc 09 §4 behaviour 2. Both tests are case-insensitive. */
export function isDroppableText(str: string): boolean {
  const lowercaseStr = str.toLowerCase();
  return (
    lowercaseStr.startsWith("http:") ||
    lowercaseStr.startsWith("https:") ||
    lowercaseStr.startsWith("magnet:") ||
    (str.length === 40 && !/[^0-9A-F]/i.test(str)) || // v1 hex info-hash
    (str.length === 32 && !/[^2-7A-Z]/i.test(str)) // v1 base32 info-hash
  );
}

/** RFC 4648 base32 (no padding) to lowercase hex; null on an invalid character. */
function base32ToHex(value: string): string | null {
  let accumulator = 0;
  let bits = 0;
  const bytes: number[] = [];
  for (const char of value.toUpperCase()) {
    const index = base32Alphabet.indexOf(char);
    if (index < 0) return null;
    accumulator = (accumulator << 5) | index;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      bytes.push((accumulator >> bits) & 0xff);
    }
  }
  return bytes.map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** The infohashes a line can key on: a bare hash, or the xt topics of a magnet. */
function infohashKeys(line: string): string[] {
  const keys: string[] = [];
  const pushHash = (value: string) => {
    if (hexInfohash.test(value)) keys.push(value.toLowerCase());
    else if (base32Infohash.test(value)) {
      const hex = base32ToHex(value);
      if (hex) keys.push(hex);
    }
  };
  if (hexInfohash.test(line) || base32Infohash.test(line)) {
    pushHash(line);
    return keys;
  }
  let url: URL;
  try {
    url = new URL(line);
  } catch {
    return keys;
  }
  if (url.protocol !== "magnet:") return keys;
  for (const xt of url.searchParams.getAll("xt")) {
    const lower = xt.toLowerCase();
    if (lower.startsWith("urn:btih:")) pushHash(xt.slice(9));
    else if (
      lower.startsWith("urn:btmh:1220") &&
      /^[0-9a-f]{64}$/i.test(lower.slice(13))
    )
      keys.push(lower.slice(13));
  }
  return keys;
}

/** Whether a URI line is a BitTorrent source (magnet or bare infohash). */
function isTorrentSource(line: string): boolean {
  return (
    line.toLowerCase().startsWith("magnet:") || infohashKeys(line).length > 0
  );
}

export function classifyLine(
  line: string,
  known: ReadonlySet<string>,
): LineBadge {
  const trimmed = line.trim();
  if (trimmed === "") return "unknown";
  if (known.has(trimmed)) return "duplicate";
  const hashes = infohashKeys(trimmed);
  if (hashes.some((hash) => known.has(hash))) return "duplicate";
  return recognisedScheme.test(trimmed) || hashes.length > 0 ? "ok" : "unknown";
}

/** The non-empty, non-comment lines a .txt part contributes to uris (doc 05 §5.2). */
export function textListLines(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"));
}

/** Lines of a dropped or pasted text payload, kept by the drop predicate (doc 09 §4). */
export function droppableLines(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && isDroppableText(line));
}

/** Doc 09 §4: a dropped directory aborts the whole drop. */
export function dropHasDirectory(data: DataTransfer): boolean {
  const items = data.items ? [...data.items] : [];
  return items.some(
    (item) =>
      item.kind === "file" && item.webkitGetAsEntry?.()?.isDirectory === true,
  );
}

export interface DropExpansion {
  /** Non-comment lines read from dropped .txt files. */
  uris: string[];
  /** .torrent/.metalink file parts for the submission. */
  files: File[];
  /** Files refused as NZB. */
  nzb: string[];
  /** Files refused for any other reason. */
  unsupported: string[];
}

/** Classifies a drop: .txt expands client-side, .torrent/.metalink stay file
 *  parts, .nzb is refused outright and anything else is unsupported. */
export async function expandDroppedFiles(
  list: Iterable<File>,
): Promise<DropExpansion> {
  const result: DropExpansion = {
    uris: [],
    files: [],
    nzb: [],
    unsupported: [],
  };
  for (const file of list) {
    const name = file.name.toLowerCase();
    if (name.endsWith(".nzb")) {
      result.nzb.push(file.name);
    } else if (name.endsWith(".txt")) {
      result.uris.push(...textListLines(await file.text()));
    } else if (
      name.endsWith(".torrent") ||
      name.endsWith(".metalink") ||
      name.endsWith(".meta4")
    ) {
      result.files.push(file);
    } else {
      result.unsupported.push(file.name);
    }
  }
  return result;
}

/** The last value handled by the clipboard toast; per session, per doc 09 §4. */
const clipboardHandledKey = "dl.addTask.clipboard";

export function markClipboardHandled(value: string): void {
  try {
    sessionStorage.setItem(clipboardHandledKey, value);
  } catch {
    // sessionStorage can be unavailable; the toast simply re-arms.
  }
}

export function clipboardHandled(value: string): boolean {
  try {
    return sessionStorage.getItem(clipboardHandledKey) === value;
  } catch {
    return false;
  }
}

function problemDetail(error: Problem | undefined, fallback: string): string {
  // `type` is an RFC 7807 URI, not user copy — it never surfaces in a toast.
  return error?.detail ?? error?.title ?? fallback;
}

/** A placeholder row shown while a URI-only submission is in flight (doc 09
 *  §10.6: URI submissions are optimistic, magnets never are). */
function placeholderTask(
  uri: string,
  index: number,
  draft: AddTaskDraft,
): Task {
  const torrent = isTorrentSource(uri);
  const lower = uri.toLowerCase();
  const kind = torrent
    ? "magnet"
    : lower.startsWith("ftp://") || lower.startsWith("ftps://")
      ? "ftp"
      : lower.startsWith("sftp://")
        ? "sftp"
        : "http";
  const now = new Date().toISOString();
  return {
    id: `optimistic-${Date.now()}-${index}`,
    name: uri,
    engine: torrent ? "qbittorrent" : "aria2",
    source_kind: kind,
    source_uri: uri,
    state: draft.paused ? "paused" : "queued",
    category: draft.category,
    tags: draft.tags,
    destination: draft.destination,
    requested_destination: null,
    content_path: null,
    infohash_v1: infohashKeys(uri).find((hash) => hash.length === 40) ?? null,
    infohash_v2: infohashKeys(uri).find((hash) => hash.length === 64) ?? null,
    error_code: null,
    error_message: null,
    total_bytes: 0,
    completed_bytes: 0,
    uploaded_bytes: 0,
    progress: 0,
    download_rate: 0,
    upload_rate: 0,
    eta_seconds: null,
    ratio: 0,
    total_peers: 0,
    connected_seeders: 0,
    connected_leechers: 0,
    dl_limit: draft.dl_limit,
    ul_limit: draft.ul_limit,
    ratio_limit: null,
    seeding_time_limit: null,
    sequential: draft.sequential,
    queue_position: null,
    unzip_progress: null,
    file_count: null,
    added_at: now,
    updated_at: now,
    started_at: null,
    completed_at: null,
  };
}

function removePlaceholderIds(ids: string[]) {
  const state = useTasks.getState();
  state.applySync({
    rid: state.rid,
    full_update: false,
    seq_gap: false,
    tasks: {},
    tasks_removed: ids,
    stats: state.stats,
  });
}

function createBody(
  draft: AddTaskDraft,
  selection: SelectionRow[] | null,
): CreateBody {
  const body: CreateBody = {
    uris: draft.uris.length > 0 ? draft.uris : null,
    paused: draft.paused,
    sequential: draft.sequential,
    create_subfolder: draft.create_subfolder,
    tags: draft.tags,
  };
  if (draft.destination !== "") body.destination = draft.destination;
  if (draft.category !== null) body.category = draft.category;
  if (draft.ftp_credentials !== null)
    body.ftp_credentials = draft.ftp_credentials;
  if (draft.extract_password !== null)
    body.extract_password = draft.extract_password;
  if (selection !== null && selection.length > 0)
    body.select_files = selection as FileSelectionRequest[];
  return body;
}

function submissionForm(
  body: CreateBody | InspectBody,
  files: File[],
): FormData {
  const form = new FormData();
  form.set("payload", JSON.stringify(body));
  for (const file of files) form.append("file", file, file.name);
  return form;
}

export function AddTaskDialog({
  open,
  onOpenChange,
  initialUris,
  initialFiles,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  initialUris?: string[];
  initialFiles?: File[];
}): JSX.Element {
  const { t, i18n } = useTranslation("dialogs");
  const locale = i18n.language;
  const queryClient = useQueryClient();

  const [urisText, setUrisText] = useState("");
  const [files, setFiles] = useState<File[]>([]);
  const [destination, setDestination] = useState("");
  const [category, setCategory] = useState<string | null>(null);
  const [categoryOpen, setCategoryOpen] = useState(false);
  const [newCategory, setNewCategory] = useState({ name: "", savePath: "" });
  const [tags, setTags] = useState<string[]>([]);
  const [tagDraft, setTagDraft] = useState("");
  const [paused, setPaused] = useState(false);
  const [sequential, setSequential] = useState(false);
  const [createSubfolder, setCreateSubfolder] = useState(false);
  const [authRequired, setAuthRequired] = useState(false);
  const [ftpUser, setFtpUser] = useState("");
  const [ftpPass, setFtpPass] = useState("");
  const [showFtpPass, setShowFtpPass] = useState(false);
  const [extractPassword, setExtractPassword] = useState("");
  const [dlLimit, setDlLimit] = useState("0");
  const [ulLimit, setUlLimit] = useState("0");
  const [selectFiles, setSelectFiles] = useState(false);
  const [moreOpen, setMoreOpen] = useState(false);
  const [browseOpen, setBrowseOpen] = useState(false);
  const [inspecting, setInspecting] = useState(false);
  const [creatingCategory, setCreatingCategory] = useState(false);
  const [gutterScroll, setGutterScroll] = useState(0);
  const [step, setStep] = useState<"add" | "select">("add");
  const [manifests, setManifests] = useState<Manifest[]>([]);
  const [selectDraft, setSelectDraft] = useState<AddTaskDraft | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const tasks = useTasks((state) => state.tasks);
  const known = useMemo(() => {
    const set = new Set<string>();
    for (const task of tasks.values()) {
      if (task.source_uri !== null) set.add(task.source_uri);
      if (task.infohash_v1 !== null) set.add(task.infohash_v1);
      if (task.infohash_v2 !== null) set.add(task.infohash_v2);
    }
    return set;
  }, [tasks]);

  // Seeding reads the props once per open: later initialUris changes belong
  // to the next opening, not to the draft the user is editing.
  const seed = useRef({ uris: initialUris, files: initialFiles });
  seed.current = { uris: initialUris, files: initialFiles };
  useEffect(() => {
    if (!open) return;
    const prefs = useUiPrefs.getState();
    const remember =
      (prefs as { rememberLastDestination?: unknown })
        .rememberLastDestination === true;
    setStep("add");
    setManifests([]);
    setSelectDraft(null);
    setUrisText((seed.current.uris ?? []).join("\n"));
    setFiles(seed.current.files ?? []);
    setDestination(remember ? (prefs.lastDestination ?? "") : "");
    setCategory(null);
    setCategoryOpen(false);
    setTags([]);
    setTagDraft("");
    setPaused(false);
    setSequential(false);
    setCreateSubfolder(false);
    setAuthRequired(false);
    setFtpUser("");
    setFtpPass("");
    setShowFtpPass(false);
    setExtractPassword("");
    setDlLimit("0");
    setUlLimit("0");
    setSelectFiles(false);
    setMoreOpen(false);
    setInspecting(false);
    setCreatingCategory(false);
    setGutterScroll(0);
  }, [open]);

  const rawLines = useMemo(() => urisText.split("\n"), [urisText]);
  const lines = useMemo(
    () => rawLines.map((line) => line.trim()).filter((line) => line !== ""),
    [rawLines],
  );
  const overCap = lines.length > uriCap;

  const ftpEnabled = useMemo(
    () => lines.some((line) => line.toLowerCase().startsWith("ftp://")),
    [lines],
  );
  const canSelectFiles = useMemo(
    () => files.length > 0 || lines.some(isTorrentSource),
    [files, lines],
  );

  const categoriesQuery = useQuery({
    queryKey: ["categories"],
    enabled: open,
    queryFn: async () => {
      const { data } = await api.GET("/categories");
      return data?.categories ?? [];
    },
  });
  const tagsQuery = useQuery({
    queryKey: ["tags"],
    enabled: open,
    queryFn: async () => {
      const { data } = await api.GET("/tags");
      return data?.tags ?? [];
    },
  });
  // The free-space line refreshes on every destination change (doc 09 §4).
  const spaceQuery = useQuery({
    queryKey: ["fs-free-space", destination],
    enabled: open && destination !== "",
    queryFn: async () => {
      const { data } = await api.GET("/fs/free-space", {
        params: { query: { path: destination } },
      });
      return data ?? null;
    },
  });
  const rootsQuery = useQuery({
    queryKey: ["fs-roots"],
    enabled: open && destination === "",
    queryFn: async () => {
      const { data } = await api.GET("/fs/roots");
      return data?.roots?.[0] ?? null;
    },
  });
  const space =
    destination === ""
      ? rootsQuery.data
        ? {
            free: rootsQuery.data.free_bytes,
            total: rootsQuery.data.total_bytes,
          }
        : null
      : spaceQuery.data
        ? {
            free: spaceQuery.data.free_bytes,
            total: spaceQuery.data.total_bytes,
          }
        : null;

  const parseLimit = (value: string): number => {
    const parsed = Math.floor(Number(value));
    return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
  };

  const draftOf = useCallback(
    (uris: string[]): AddTaskDraft => ({
      uris,
      files,
      destination,
      category,
      tags,
      paused,
      sequential,
      create_subfolder: createSubfolder,
      ftp_credentials:
        authRequired && ftpEnabled
          ? { username: ftpUser, password: ftpPass }
          : null,
      extract_password: extractPassword !== "" ? extractPassword : null,
      dl_limit: parseLimit(dlLimit),
      ul_limit: parseLimit(ulLimit),
    }),
    [
      files,
      destination,
      category,
      tags,
      paused,
      sequential,
      createSubfolder,
      authRequired,
      ftpEnabled,
      ftpUser,
      ftpPass,
      extractPassword,
      dlLimit,
      ulLimit,
    ],
  );

  const applyLimits = async (created: TaskDTO[], draft: AddTaskDraft) => {
    const body: components["schemas"]["PatchTaskBody"] = {};
    if (draft.dl_limit > 0) body.dl_limit = draft.dl_limit;
    if (draft.ul_limit > 0) body.ul_limit = draft.ul_limit;
    if (body.dl_limit === undefined && body.ul_limit === undefined) return;
    // The create body has no limit fields (doc 05 §5.2): non-zero limits are
    // patched onto each created task (§5.5). A failed PATCH never rolls back
    // a created task — the row stays and a toast names it.
    for (const task of created) {
      const { error } = await api.PATCH("/tasks/{id}", {
        params: { path: { id: task.id } },
        body,
      });
      if (error)
        toast.error(
          t("addTask.limitFailed", {
            name: task.name,
            detail: problemDetail(error, t("shell.networkError")),
          }),
        );
    }
  };

  const postTasks = async (
    draft: AddTaskDraft,
    selection: SelectionRow[] | null,
  ) => {
    const body = createBody(draft, selection);
    if (draft.files.length === 0) return api.POST("/tasks", { body });
    return api.POST("/tasks", {
      body: { payload: JSON.stringify(body) },
      bodySerializer: () => submissionForm(body, draft.files),
    });
  };

  const create = async (
    draft: AddTaskDraft,
    selection: SelectionRow[] | null,
  ) => {
    onOpenChange(false);
    // Doc 09 §4.4 and §10.6: URI submissions are optimistic unless they carry
    // a magnet; uploads and magnets never get placeholder rows.
    const optimistic =
      draft.files.length === 0 &&
      draft.uris.length > 0 &&
      draft.uris.every((uri) => !isTorrentSource(uri));
    const placeholders = optimistic
      ? draft.uris.map((uri, index) => placeholderTask(uri, index, draft))
      : [];
    if (placeholders.length > 0) useTasks.getState().hydrate(placeholders);
    try {
      const { data, error } = await postTasks(draft, selection);
      if (!data) throw new Error(problemDetail(error, t("shell.networkError")));
      if (placeholders.length > 0)
        removePlaceholderIds(placeholders.map((task) => task.id));
      const created = data.created ?? [];
      if (created.length > 0) {
        useTasks.getState().hydrate(created);
        if (draft.destination !== "")
          useUiPrefs.getState().patch({ lastDestination: draft.destination });
        void queryClient.invalidateQueries({ queryKey: ["tasks"] });
        await applyLimits(created, draft);
      }
      for (const rejected of data.rejected ?? [])
        toast.error(
          t("addTask.rejected", {
            uri: rejected.uri,
            detail: rejected.detail,
          }),
        );
    } catch (error) {
      if (placeholders.length > 0)
        removePlaceholderIds(placeholders.map((task) => task.id));
      toast.error(
        t("addTask.submitFailed", {
          uri: draft.uris[0] ?? draft.files[0]?.name ?? "",
          detail:
            error instanceof Error ? error.message : t("shell.networkError"),
        }),
      );
    }
  };

  /** Over-50 submissions go out in batches of MAX_URIS (doc 09 §4 behaviour 1);
   *  the file parts ride with the first batch. */
  const createBatches = async () => {
    const batches: string[][] = [];
    for (let i = 0; i < lines.length; i += uriCap)
      batches.push(lines.slice(i, i + uriCap));
    onOpenChange(false);
    for (const [index, batch] of batches.entries()) {
      const draft = draftOf(batch);
      if (index > 0) draft.files = [];
      const body = createBody(draft, null);
      try {
        const { data, error } =
          draft.files.length === 0
            ? await api.POST("/tasks", { body })
            : await api.POST("/tasks", {
                body: { payload: JSON.stringify(body) },
                bodySerializer: () => submissionForm(body, draft.files),
              });
        if (!data)
          throw new Error(problemDetail(error, t("shell.networkError")));
        if (data.created?.length) {
          useTasks.getState().hydrate(data.created);
          void applyLimits(data.created, draft);
        }
        for (const rejected of data.rejected ?? [])
          toast.error(
            t("addTask.rejected", {
              uri: rejected.uri,
              detail: rejected.detail,
            }),
          );
      } catch (error) {
        toast.error(
          t("addTask.submitFailed", {
            uri: batch[0] ?? "",
            detail:
              error instanceof Error ? error.message : t("shell.networkError"),
          }),
        );
      }
    }
    if (destination !== "")
      useUiPrefs.getState().patch({ lastDestination: destination });
    void queryClient.invalidateQueries({ queryKey: ["tasks"] });
  };

  const inspect = async (draft: AddTaskDraft) => {
    setInspecting(true);
    try {
      const body: InspectBody = {
        uris: draft.uris.length > 0 ? draft.uris : null,
      };
      const { data, error } =
        draft.files.length === 0
          ? await api.POST("/tasks/inspect", { body })
          : await api.POST("/tasks/inspect", {
              body: { payload: JSON.stringify(body) },
              bodySerializer: () => submissionForm(body, draft.files),
            });
      if (!data) {
        toast.error(
          t("addTask.inspectFailed", {
            detail: problemDetail(error, t("shell.networkError")),
          }),
        );
        return;
      }
      for (const rejected of data.rejected ?? [])
        toast.error(
          t("addTask.rejected", {
            uri: rejected.uri,
            detail: rejected.detail,
          }),
        );
      const manifests = data.manifests ?? [];
      // Every source rejected means there is nothing to select: the step
      // stays on the add page with the rejected toasts already shown.
      if (manifests.length === 0) return;
      setSelectDraft(draft);
      setManifests(manifests);
      setStep("select");
    } catch (error) {
      toast.error(
        t("addTask.inspectFailed", {
          detail:
            error instanceof Error ? error.message : t("shell.networkError"),
        }),
      );
    } finally {
      setInspecting(false);
    }
  };

  const submit = () => {
    if (lines.length === 0 && files.length === 0) return;
    const draft = draftOf(lines.slice(0, uriCap));
    if (selectFiles && canSelectFiles) void inspect(draft);
    else void create(draft, null);
  };

  const onUrisKeyDown = (event: ReactKeyboardEvent<HTMLTextAreaElement>) => {
    // Doc 09 §4.4: Enter inserts a newline; Ctrl/Cmd+Enter submits.
    if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      submit();
    }
  };

  const reportExpansion = (expansion: DropExpansion) => {
    expansion.nzb.forEach(() => toast.error(t("addTask.nzb")));
    for (const name of expansion.unsupported)
      toast.error(t("addTask.unsupportedFile", { name }));
  };

  const onDropzoneDrop = async (event: ReactDragEvent<HTMLDivElement>) => {
    event.preventDefault();
    if (dropHasDirectory(event.dataTransfer)) {
      toast.error(t("addTask.dirDropped"));
      return;
    }
    const expansion = await expandDroppedFiles(event.dataTransfer.files);
    reportExpansion(expansion);
    if (expansion.uris.length > 0)
      setUrisText((text) =>
        [text, ...expansion.uris].filter((part) => part !== "").join("\n"),
      );
    if (expansion.files.length > 0)
      setFiles((previous) => [...previous, ...expansion.files]);
  };

  const onFilesPicked = async (list: FileList | null) => {
    if (!list) return;
    const expansion = await expandDroppedFiles(list);
    reportExpansion(expansion);
    if (expansion.uris.length > 0)
      setUrisText((text) =>
        [text, ...expansion.uris].filter((part) => part !== "").join("\n"),
      );
    if (expansion.files.length > 0)
      setFiles((previous) => [...previous, ...expansion.files]);
  };

  const addTag = (name: string) => {
    const tag = name.trim().replace(/,/g, "");
    if (tag === "" || tags.includes(tag)) return;
    setTags([...tags, tag]);
    setTagDraft("");
  };

  const createCategory = async () => {
    if (creatingCategory) return;
    const name = newCategory.name.trim();
    const savePath = newCategory.savePath.trim();
    if (name === "" || savePath === "") return;
    setCreatingCategory(true);
    try {
      const { data, error } = await api.POST("/categories", {
        body: { name, save_path: savePath },
      });
      if (!data) {
        toast.error(
          t("addTask.categoryFailed", {
            detail: problemDetail(error, t("shell.networkError")),
          }),
        );
        return;
      }
      setCategory(data.name);
      setCategoryOpen(false);
      setNewCategory({ name: "", savePath: "" });
      void queryClient.invalidateQueries({ queryKey: ["categories"] });
    } finally {
      setCreatingCategory(false);
    }
  };

  const nothingToSubmit = lines.length === 0 && files.length === 0;

  const gutter = (badge: LineBadge | null, key: number): ReactNode => (
    <span key={key} className="block h-5 text-center leading-5">
      {badge === "ok" ? (
        <span className="text-[var(--ok)]">✓</span>
      ) : badge === "unknown" ? (
        <span className="text-[var(--warn,orange)]">⚠</span>
      ) : badge === "duplicate" ? (
        <span className="text-destructive">✕</span>
      ) : null}
    </span>
  );

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="sm:max-w-xl" aria-label={t("addTask.title")}>
          {step === "add" ? (
            <>
              <DialogHeader>
                <DialogTitle>{t("addTask.title")}</DialogTitle>
              </DialogHeader>

              <div className="flex items-end gap-2">
                <div className="min-w-0 flex-1">
                  <Label htmlFor="add-destination">
                    {t("addTask.destination")}
                  </Label>
                  <Input
                    id="add-destination"
                    readOnly
                    value={destination}
                    placeholder={t("addTask.destinationDefault")}
                  />
                  <p className="mt-0.5 text-sm text-muted-foreground tabular-nums">
                    {space
                      ? t("folderBrowser.freeSpace", {
                          free: formatBytes(space.free, locale),
                          total: formatBytes(space.total, locale),
                        })
                      : null}
                  </p>
                </div>
                <Button variant="outline" onClick={() => setBrowseOpen(true)}>
                  {t("addTask.selectDestination")}
                </Button>
              </div>

              <div>
                <Label htmlFor="add-uris">{t("addTask.uriLabel")}</Label>
                <div className="relative">
                  <div
                    aria-hidden="true"
                    className="pointer-events-none absolute top-1 bottom-1 left-1.5 w-4 overflow-hidden font-mono text-sm"
                  >
                    <div
                      style={{ transform: `translateY(${-gutterScroll}px)` }}
                    >
                      {rawLines.map((line, index) =>
                        gutter(
                          line.trim() === "" ? null : classifyLine(line, known),
                          index,
                        ),
                      )}
                    </div>
                  </div>
                  <textarea
                    id="add-uris"
                    rows={6}
                    spellCheck={false}
                    autoCapitalize="off"
                    autoCorrect="off"
                    // Soft wrap would stack a logical line over several visual
                    // rows and break the one-badge-per-line gutter alignment.
                    wrap="off"
                    value={urisText}
                    onChange={(event) => setUrisText(event.target.value)}
                    onScroll={(event) =>
                      setGutterScroll(event.currentTarget.scrollTop)
                    }
                    onKeyDown={onUrisKeyDown}
                    className="w-full rounded-lg border border-input bg-transparent py-1 pr-2 pl-7 font-mono text-sm leading-5 outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
                  />
                </div>
                <div className="mt-0.5 flex items-start justify-between gap-2 text-sm text-muted-foreground">
                  <span>{t("addTask.uriHelp", { max: uriCap })}</span>
                  <span className="shrink-0 text-right tabular-nums">
                    {overCap
                      ? t("addTask.lineCounterOver", {
                          total: lines.length,
                          max: uriCap,
                        })
                      : t("addTask.lineCounter", {
                          total: lines.length,
                          max: uriCap,
                        })}
                    {overCap ? (
                      <Button
                        variant="link"
                        size="sm"
                        className="ml-1 h-auto p-0"
                        onClick={() => void createBatches()}
                      >
                        {t("addTask.splitBatches")}
                      </Button>
                    ) : null}
                  </span>
                </div>
              </div>

              <div
                onDragOver={(event) => event.preventDefault()}
                onDrop={(event) => void onDropzoneDrop(event)}
              >
                <div
                  role="button"
                  tabIndex={0}
                  aria-label={t("addTask.dropzone")}
                  onClick={() => fileInput.current?.click()}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      fileInput.current?.click();
                    }
                  }}
                  className="rounded-lg border border-dashed border-input px-3 py-2 text-sm text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
                >
                  <span className="flex items-center gap-2">
                    <Upload className="size-4" aria-hidden="true" />
                    {files.length === 0
                      ? t("addTask.dropzone")
                      : t("addTask.dropzoneFiles", { total: files.length })}
                  </span>
                </div>
                {files.length > 0 ? (
                  // Interactive controls never nest inside the role=button
                  // dropzone; the picked-file list sits beside it.
                  <ul className="mt-1 flex flex-col gap-1 text-sm">
                    {files.map((file, index) => (
                      <li
                        key={`${file.name}-${index}`}
                        className="flex items-center gap-2 text-foreground"
                      >
                        <span
                          className="min-w-0 flex-1 truncate"
                          title={file.name}
                        >
                          {file.name}
                        </span>
                        <span className="shrink-0 tabular-nums text-muted-foreground">
                          ({formatBytes(file.size, locale)})
                        </span>
                        <button
                          type="button"
                          aria-label={t("addTask.removeFile", {
                            name: file.name,
                          })}
                          onClick={() =>
                            setFiles((previous) =>
                              previous.filter((_, i) => i !== index),
                            )
                          }
                        >
                          <X className="size-3.5" aria-hidden="true" />
                        </button>
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
              <input
                ref={fileInput}
                type="file"
                multiple
                hidden
                accept=".torrent,.txt"
                aria-label={t("addTask.browse")}
                onChange={(event) => {
                  void onFilesPicked(event.target.files);
                  event.target.value = "";
                }}
              />

              <div>
                <label
                  className={`flex items-center gap-2 text-sm ${ftpEnabled ? "" : "opacity-50"}`}
                  title={ftpEnabled ? undefined : t("addTask.ftpDisabledTip")}
                >
                  <Checkbox
                    checked={authRequired}
                    disabled={!ftpEnabled}
                    onCheckedChange={(checked) =>
                      setAuthRequired(checked === true)
                    }
                  />
                  {t("addTask.ftpAuth")}
                </label>
                {authRequired && ftpEnabled ? (
                  <div className="mt-1 flex items-center gap-2 pl-6">
                    <Input
                      aria-label={t("addTask.username")}
                      placeholder={t("addTask.username")}
                      value={ftpUser}
                      onChange={(event) => setFtpUser(event.target.value)}
                    />
                    <span className="relative flex-1">
                      <Input
                        type={showFtpPass ? "text" : "password"}
                        aria-label={t("addTask.password")}
                        placeholder={t("addTask.password")}
                        value={ftpPass}
                        onChange={(event) => setFtpPass(event.target.value)}
                      />
                      <button
                        type="button"
                        className="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground"
                        aria-label={
                          showFtpPass
                            ? t("addTask.hidePassword")
                            : t("addTask.showPassword")
                        }
                        onClick={() => setShowFtpPass((show) => !show)}
                      >
                        {showFtpPass ? (
                          <EyeOff className="size-4" aria-hidden="true" />
                        ) : (
                          <Eye className="size-4" aria-hidden="true" />
                        )}
                      </button>
                    </span>
                  </div>
                ) : null}
              </div>

              <label
                className={`flex items-center gap-2 text-sm ${canSelectFiles ? "" : "opacity-50"}`}
                title={
                  canSelectFiles
                    ? undefined
                    : t("addTask.selectFilesDisabledTip")
                }
              >
                <Checkbox
                  checked={selectFiles}
                  disabled={!canSelectFiles}
                  onCheckedChange={(checked) =>
                    setSelectFiles(checked === true)
                  }
                />
                {t("addTask.selectFiles")}
              </label>

              <div>
                <button
                  type="button"
                  aria-expanded={moreOpen}
                  className="flex items-center gap-1 text-sm"
                  onClick={() => setMoreOpen((value) => !value)}
                >
                  {moreOpen ? (
                    <ChevronDown className="size-4" aria-hidden="true" />
                  ) : (
                    <ChevronRight className="size-4" aria-hidden="true" />
                  )}
                  {t("addTask.moreOptions")}
                </button>
                {moreOpen ? (
                  <div className="mt-2 flex flex-col gap-2 pl-5">
                    <div className="flex flex-wrap items-center gap-2">
                      <Label htmlFor="add-category">
                        {t("addTask.category")}
                      </Label>
                      <Select
                        // Radix rejects an empty-string item value, so the
                        // "none" item rides a sentinel. A real category named
                        // after either sentinel still wins — the sentinel only
                        // acts when no category bears its name.
                        value={category ?? "__none__"}
                        onValueChange={(value) => {
                          const names = new Set(
                            (categoriesQuery.data ?? []).map(
                              (item) => item.name,
                            ),
                          );
                          if (value === "__new__" && !names.has("__new__")) {
                            setNewCategory({
                              name: "",
                              savePath: destination,
                            });
                            setCategoryOpen(true);
                          } else if (
                            value === "__none__" &&
                            !names.has("__none__")
                          ) {
                            setCategory(null);
                          } else {
                            setCategory(value);
                          }
                        }}
                      >
                        <SelectTrigger
                          id="add-category"
                          size="sm"
                          className="w-44"
                        >
                          <SelectValue
                            placeholder={t("addTask.categoryNone")}
                          />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="__none__">
                            {t("addTask.categoryNone")}
                          </SelectItem>
                          {(categoriesQuery.data ?? []).map((item) => (
                            <SelectItem key={item.name} value={item.name}>
                              {item.name}
                            </SelectItem>
                          ))}
                          <SelectItem value="__new__">
                            {t("addTask.categoryNew")}
                          </SelectItem>
                        </SelectContent>
                      </Select>
                      <Label htmlFor="add-tag">{t("addTask.tags")}</Label>
                      <span className="flex min-w-0 flex-1 flex-wrap items-center gap-1">
                        {tags.map((tag) => (
                          <span
                            key={tag}
                            className="flex items-center gap-0.5 rounded bg-muted px-1.5 py-0.5 text-xs"
                          >
                            {tag}
                            <button
                              type="button"
                              aria-label={t("addTask.removeTag", {
                                name: tag,
                              })}
                              onClick={() =>
                                setTags(tags.filter((item) => item !== tag))
                              }
                            >
                              <X className="size-3" aria-hidden="true" />
                            </button>
                          </span>
                        ))}
                        <Input
                          id="add-tag"
                          className="h-7 w-28"
                          placeholder={t("addTask.tagPlaceholder")}
                          value={tagDraft}
                          onChange={(event) => setTagDraft(event.target.value)}
                          onKeyDown={(event) => {
                            if (event.key === "Enter" || event.key === ",") {
                              event.preventDefault();
                              addTag(tagDraft);
                            }
                          }}
                        />
                      </span>
                    </div>
                    {tagDraft !== "" ? (
                      <span className="flex flex-wrap gap-1">
                        {(tagsQuery.data ?? [])
                          .filter(
                            (item) =>
                              !tags.includes(item.name) &&
                              item.name
                                .toLowerCase()
                                .includes(tagDraft.toLowerCase()),
                          )
                          .slice(0, 5)
                          .map((item) => (
                            <button
                              key={item.name}
                              type="button"
                              className="rounded bg-muted px-1.5 py-0.5 text-xs hover:bg-accent"
                              onClick={() => addTag(item.name)}
                            >
                              {item.name}
                            </button>
                          ))}
                      </span>
                    ) : null}
                    {categoryOpen ? (
                      <div className="flex flex-wrap items-end gap-2 rounded-md border border-border p-2">
                        <span>
                          <Label htmlFor="new-category-name">
                            {t("addTask.categoryName")}
                          </Label>
                          <Input
                            id="new-category-name"
                            className="h-7 w-36"
                            value={newCategory.name}
                            onChange={(event) =>
                              setNewCategory({
                                ...newCategory,
                                name: event.target.value,
                              })
                            }
                          />
                        </span>
                        <span>
                          <Label htmlFor="new-category-path">
                            {t("addTask.categoryPath")}
                          </Label>
                          <Input
                            id="new-category-path"
                            className="h-7 w-48"
                            value={newCategory.savePath}
                            onChange={(event) =>
                              setNewCategory({
                                ...newCategory,
                                savePath: event.target.value,
                              })
                            }
                          />
                        </span>
                        <Button
                          size="sm"
                          disabled={
                            creatingCategory ||
                            newCategory.name.trim() === "" ||
                            newCategory.savePath.trim() === ""
                          }
                          onClick={() => void createCategory()}
                        >
                          {t("addTask.categoryCreate")}
                        </Button>
                      </div>
                    ) : null}
                    <div className="flex flex-wrap items-center gap-4">
                      <label className="flex items-center gap-2 text-sm">
                        <Checkbox
                          checked={paused}
                          onCheckedChange={(checked) =>
                            setPaused(checked === true)
                          }
                        />
                        {t("addTask.addPaused")}
                      </label>
                      <label className="flex items-center gap-2 text-sm">
                        <Checkbox
                          checked={sequential}
                          onCheckedChange={(checked) =>
                            setSequential(checked === true)
                          }
                        />
                        {t("addTask.sequential")}
                      </label>
                    </div>
                    <div className="flex flex-wrap items-center gap-4">
                      <label className="flex items-center gap-2 text-sm">
                        {t("addTask.downloadLimit")}
                        <Input
                          type="number"
                          min={0}
                          step={1}
                          className="h-7 w-24"
                          aria-label={t("addTask.downloadLimit")}
                          value={dlLimit}
                          onChange={(event) => setDlLimit(event.target.value)}
                        />
                        {t("addTask.bytesPerSecond")}
                      </label>
                      <label className="flex items-center gap-2 text-sm">
                        {t("addTask.uploadLimit")}
                        <Input
                          type="number"
                          min={0}
                          step={1}
                          className="h-7 w-24"
                          aria-label={t("addTask.uploadLimit")}
                          value={ulLimit}
                          onChange={(event) => setUlLimit(event.target.value)}
                        />
                        {t("addTask.bytesPerSecond")}
                      </label>
                    </div>
                    <label
                      className="flex items-center gap-2 text-sm"
                      title={t("addTask.subfolderTip")}
                    >
                      <Checkbox
                        checked={createSubfolder}
                        onCheckedChange={(checked) =>
                          setCreateSubfolder(checked === true)
                        }
                      />
                      {t("addTask.subfolder")}
                    </label>
                    <label className="flex items-center gap-2 text-sm">
                      {t("addTask.extractPassword")}
                      <Input
                        type="password"
                        className="h-7 w-48"
                        aria-label={t("addTask.extractPassword")}
                        value={extractPassword}
                        onChange={(event) =>
                          setExtractPassword(event.target.value)
                        }
                      />
                    </label>
                  </div>
                ) : null}
              </div>

              <DialogFooter>
                <Button variant="outline" onClick={() => onOpenChange(false)}>
                  {t("addTask.cancel")}
                </Button>
                <Button
                  disabled={nothingToSubmit || inspecting}
                  onClick={submit}
                >
                  {selectFiles && canSelectFiles
                    ? t("addTask.next")
                    : t("addTask.create")}
                </Button>
              </DialogFooter>
            </>
          ) : (
            selectDraft && (
              <FileSelectionDialog
                manifests={manifests}
                draft={selectDraft}
                onBack={() => setStep("add")}
                onCreate={(draft, selection) => void create(draft, selection)}
              />
            )
          )}
        </DialogContent>
      </Dialog>
      <FolderBrowserDialog
        open={browseOpen}
        initialPath={destination !== "" ? destination : undefined}
        onOpenChange={setBrowseOpen}
        onSelect={(path) => setDestination(path)}
      />
    </>
  );
}
