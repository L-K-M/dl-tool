import {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type JSX,
  type KeyboardEvent,
} from "react";
import { ChevronDown, ChevronRight, FileIcon, FolderIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { initI18n } from "../../i18n";
import { formatBytes, formatPercent } from "../../lib/format";

initI18n();

export type FilePriority = "skip" | "normal" | "high" | "maximum";

export interface FileNode {
  index: number | null; // null for a folder
  path: string; // relative to the task destination
  name: string;
  size: number | null;
  progress?: number;
  selected: boolean | "mixed";
  priority: FilePriority | null; // null for an aria2 task, which has no priorities
  children?: FileNode[];
}

export interface FileTreeProps {
  nodes: FileNode[];
  /** Read-only for a single-file HTTP or FTP task. */
  readOnly?: boolean;
  filter?: string; // substring, plus ext:mkv and >100MB
  /** Free bytes on the destination filesystem, for the footer total. */
  freeBytes?: number | null;
  onChange: (changes: FileChange[]) => void;
}

export interface FileChange {
  index: number;
  selected?: boolean;
  priority?: FilePriority;
}

/** The select's four options, in doc 09 §5 order. There is no Low. */
const PRIORITIES: FilePriority[] = ["skip", "normal", "high", "maximum"];
const SIZE_UNITS: Record<string, number> = {
  b: 1,
  kb: 1024,
  mb: 1024 ** 2,
  gb: 1024 ** 3,
  tb: 1024 ** 4,
};
const INDENT = 16;

interface FileInput {
  index: number;
  path: string;
  size_bytes: number | null;
  selected: boolean;
  priority: FilePriority | null;
  progress?: number;
}

/** Builds the tree from a flat files[] list, folding common path prefixes into folders. */
export function buildTree(files: FileInput[]): FileNode[] {
  const roots: FileNode[] = [];
  const folders = new Map<string, FileNode>();
  for (const file of files) {
    const segments = file.path.split("/").filter((segment) => segment !== "");
    let siblings = roots;
    let prefix = "";
    for (const segment of segments.slice(0, -1)) {
      prefix = prefix === "" ? segment : `${prefix}/${segment}`;
      let folder = folders.get(prefix);
      if (!folder) {
        folder = {
          index: null,
          path: prefix,
          name: segment,
          size: null,
          selected: false,
          priority: null,
          children: [],
        };
        folders.set(prefix, folder);
        siblings.push(folder);
      }
      siblings = folder.children ?? [];
    }
    siblings.push({
      index: file.index,
      path: file.path,
      name: segments[segments.length - 1] ?? file.path,
      size: file.size_bytes,
      progress: file.progress,
      selected: file.selected,
      priority: file.priority,
    });
  }
  // Roll leaf values into their folders, post-order.
  const finish = (
    node: FileNode,
  ): { count: number; selected: number; size: number | null } => {
    if (!node.children) {
      return {
        count: 1,
        selected: node.selected === true ? 1 : 0,
        size: node.size,
      };
    }
    let count = 0;
    let selected = 0;
    let size: number | null = 0;
    const priorities = new Set<FilePriority>();
    // A null child priority means either "no priorities on this engine" or
    // "mixed descendants" — only the second must force this folder's —.
    let mixed = false;
    for (const child of node.children) {
      const rolled = finish(child);
      count += rolled.count;
      selected += rolled.selected;
      size = size === null || rolled.size === null ? null : size + rolled.size;
      if (child.priority !== null) priorities.add(child.priority);
      else if (hasPriorities(child)) mixed = true;
    }
    node.size = size;
    node.selected =
      selected === 0 ? false : selected === count ? true : "mixed";
    // A uniform descendant priority surfaces on the folder; a mixed one shows —.
    node.priority = !mixed && priorities.size === 1 ? [...priorities][0] : null;
    return { count, selected, size };
  };
  for (const root of roots) finish(root);
  return roots;
}

function leaves(node: FileNode): FileNode[] {
  if (!node.children) return [node];
  return node.children.flatMap(leaves);
}

/** Whether any descendant file declares a priority (false for aria2 tasks). */
function hasPriorities(node: FileNode): boolean {
  if (!node.children) return node.priority !== null;
  return node.children.some(hasPriorities);
}

function tokenMatches(leaf: FileNode, token: string): boolean {
  if (token.startsWith("ext:")) {
    const ext = token.slice(4);
    return ext !== "" && leaf.name.toLowerCase().endsWith(`.${ext}`);
  }
  const size = /^>(\d+(?:\.\d+)?)\s*(b|kb|mb|gb|tb)?$/.exec(token);
  if (size) {
    const unit = SIZE_UNITS[(size[2] ?? "b").toLowerCase()] ?? 1;
    return (leaf.size ?? 0) > Number(size[1]) * unit;
  }
  return leaf.path.toLowerCase().includes(token);
}

/** Applies the §5 filter: space-separated tokens AND together; a folder stays
 *  visible while any descendant file matches. */
function filterNodes(nodes: FileNode[], filter?: string): FileNode[] {
  const tokens = (filter ?? "")
    .trim()
    .toLowerCase()
    .split(/\s+/)
    .filter((token) => token !== "");
  if (tokens.length === 0) return nodes;
  const walk = (node: FileNode): FileNode | null => {
    if (!node.children) {
      return tokens.every((token) => tokenMatches(node, token)) ? node : null;
    }
    const children = node.children
      .map(walk)
      .filter((child): child is FileNode => child !== null);
    return children.length > 0 ? { ...node, children } : null;
  };
  return nodes.map(walk).filter((node): node is FileNode => node !== null);
}

interface FlatRow {
  node: FileNode;
  depth: number;
}

function flatten(nodes: FileNode[], collapsed: ReadonlySet<string>): FlatRow[] {
  const rows: FlatRow[] = [];
  const walk = (list: FileNode[], depth: number) => {
    for (const node of list) {
      rows.push({ node, depth });
      if (node.children && !collapsed.has(node.path))
        walk(node.children, depth + 1);
    }
  };
  walk(nodes, 1);
  return rows;
}

/** A real checkbox whose indeterminate state is assigned imperatively — CSS
 *  cannot express the mixed state (doc 09 §5). */
function TriCheckbox({
  state,
  disabled,
  label,
  onToggle,
}: {
  state: boolean | "mixed";
  disabled?: boolean;
  label: string;
  onToggle: () => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useLayoutEffect(() => {
    if (ref.current) ref.current.indeterminate = state === "mixed";
  }, [state]);
  return (
    <input
      ref={ref}
      type="checkbox"
      checked={state === true}
      disabled={disabled}
      aria-label={label}
      onChange={onToggle}
      onClick={(event) => event.stopPropagation()}
    />
  );
}

export function FileTree({
  nodes,
  readOnly,
  filter,
  freeBytes,
  onChange,
}: FileTreeProps): JSX.Element {
  const { t, i18n } = useTranslation();
  const locale = i18n.language;
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [focusPath, setFocusPath] = useState<string | null>(null);
  const rowRefs = useRef(new Map<string, HTMLDivElement>());

  const visible = useMemo(() => filterNodes(nodes, filter), [nodes, filter]);
  const rows = useMemo(() => flatten(visible, collapsed), [visible, collapsed]);
  // A roving tabindex always keeps exactly one tab stop: the focused row, or
  // the first row when nothing is focused or the focus was filtered away.
  const focusRow = rows.some((row) => row.node.path === focusPath)
    ? focusPath
    : rows[0]?.node.path;
  useEffect(() => {
    const row = focusPath === null ? undefined : rowRefs.current.get(focusPath);
    // Clicking a nested control bubbles focusin to the row, which sets
    // focusPath — the row already contains the focus, so don't yank it back.
    if (row && !row.contains(document.activeElement))
      row.focus({ preventScroll: true });
  }, [focusPath]);
  const all = useMemo(() => nodes.flatMap(leaves), [nodes]);
  const showProgress = all.some((node) => node.progress !== undefined);

  const selectedLeaves = all.filter((node) => node.selected === true);
  const wantedBytes = selectedLeaves.reduce(
    (sum, node) => sum + (node.size ?? 0),
    0,
  );
  const totalBytes = all.reduce((sum, node) => sum + (node.size ?? 0), 0);
  const shortfall = freeBytes != null && wantedBytes > freeBytes;

  const emitSelect = (node: FileNode, selected: boolean) => {
    // selected:false is priority skip; the select can never disagree. For a
    // re-check the server maps a bare select to normal, so no priority field.
    onChange(
      leaves(node).flatMap((leaf) =>
        leaf.index === null ? [] : [{ index: leaf.index, selected }],
      ),
    );
  };
  const emitPriority = (node: FileNode, priority: FilePriority) => {
    onChange(
      leaves(node).flatMap((leaf): FileChange[] => {
        if (leaf.index === null) return [];
        // null priority means the engine has none (aria2): drive selected only.
        return leaf.priority === null
          ? [{ index: leaf.index, selected: priority !== "skip" }]
          : [{ index: leaf.index, priority }];
      }),
    );
  };
  const toggleCollapsed = (path: string) =>
    setCollapsed((previous) => {
      const next = new Set(previous);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });

  const onTreeKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    // Nested controls keep their own keys — the priority select opens and
    // navigates with arrows, and a focused checkbox hears nothing here.
    if (
      rows.length === 0 ||
      (event.target as HTMLElement).closest("input, select, button")
    )
      return;
    const index = Math.max(
      0,
      rows.findIndex((row) => row.node.path === focusPath),
    );
    const row = rows[index];
    switch (event.key) {
      case "ArrowDown":
        setFocusPath(rows[Math.min(rows.length - 1, index + 1)].node.path);
        break;
      case "ArrowUp":
        setFocusPath(rows[Math.max(0, index - 1)].node.path);
        break;
      case "ArrowRight":
        if (row.node.children && collapsed.has(row.node.path))
          toggleCollapsed(row.node.path);
        else if (row.node.children)
          setFocusPath(rows[Math.min(rows.length - 1, index + 1)].node.path);
        break;
      case "ArrowLeft":
        if (row.node.children && !collapsed.has(row.node.path))
          toggleCollapsed(row.node.path);
        else if (index > 0) {
          // Walk up to the shallowest ancestor still visible above this row.
          const parent = rows
            .slice(0, index)
            .reverse()
            .find(
              (candidate) =>
                candidate.depth < row.depth &&
                row.node.path.startsWith(`${candidate.node.path}/`),
            );
          if (parent) setFocusPath(parent.node.path);
        }
        break;
      default:
        return;
    }
    event.preventDefault();
  };

  return (
    <div className="flex min-h-0 flex-col">
      <div
        role="tree"
        aria-label={t("detail.files.treeLabel")}
        onKeyDown={onTreeKeyDown}
        className="min-h-0 flex-1 overflow-auto"
      >
        {rows.map(({ node, depth }) => {
          const folder = node.children !== undefined;
          const expanded = folder && !collapsed.has(node.path);
          const focused = focusRow === node.path;
          return (
            <div
              key={node.path}
              ref={(element) => {
                if (element) rowRefs.current.set(node.path, element);
                else rowRefs.current.delete(node.path);
              }}
              role="treeitem"
              aria-level={depth}
              aria-expanded={folder ? expanded : undefined}
              aria-checked={
                node.selected === "mixed"
                  ? "mixed"
                  : node.selected
                    ? "true"
                    : "false"
              }
              aria-selected={focused}
              tabIndex={focused ? 0 : -1}
              onFocus={() => setFocusPath(node.path)}
              onClick={() => setFocusPath(node.path)}
              className="flex items-center gap-1.5 py-0.5 pr-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
              style={{ paddingLeft: depth * INDENT }}
            >
              {folder ? (
                <button
                  type="button"
                  tabIndex={-1}
                  aria-label={t(
                    expanded ? "detail.collapse" : "detail.expand",
                    { name: node.name },
                  )}
                  onClick={(event) => {
                    event.stopPropagation();
                    toggleCollapsed(node.path);
                  }}
                  className="shrink-0"
                >
                  {expanded ? (
                    <ChevronDown className="size-3.5" aria-hidden />
                  ) : (
                    <ChevronRight className="size-3.5" aria-hidden />
                  )}
                </button>
              ) : (
                <span className="inline-block size-3.5 shrink-0" aria-hidden />
              )}
              <TriCheckbox
                state={node.selected}
                disabled={readOnly}
                label={t("detail.files.select", { name: node.name })}
                onToggle={() => emitSelect(node, node.selected !== true)}
              />
              {folder ? (
                <FolderIcon className="size-4 shrink-0" aria-hidden />
              ) : (
                <FileIcon className="size-4 shrink-0" aria-hidden />
              )}
              <span
                className="min-w-0 flex-1 truncate"
                title={node.path}
                onClick={folder ? () => toggleCollapsed(node.path) : undefined}
              >
                {node.name}
              </span>
              <span className="shrink-0 tabular-nums text-muted-foreground">
                {formatBytes(node.size, locale)}
              </span>
              {showProgress ? (
                <>
                  <span className="w-14 shrink-0 text-right tabular-nums">
                    {node.progress === undefined
                      ? "—"
                      : formatPercent(node.progress, locale)}
                  </span>
                  <span className="w-16 shrink-0 text-right tabular-nums text-muted-foreground">
                    {node.size === null || node.progress === undefined
                      ? "—"
                      : formatBytes(
                          Math.round(node.size * (1 - node.progress)),
                          locale,
                        )}
                  </span>
                </>
              ) : null}
              {hasPriorities(node) ? (
                <select
                  aria-label={t("detail.files.priority", { name: node.name })}
                  disabled={readOnly}
                  className="h-6 shrink-0 rounded-md border border-input bg-transparent px-1 text-sm"
                  value={
                    folder
                      ? (node.priority ?? "")
                      : node.selected
                        ? (node.priority ?? "normal")
                        : "skip"
                  }
                  onChange={(event) =>
                    emitPriority(node, event.target.value as FilePriority)
                  }
                  onClick={(event) => event.stopPropagation()}
                >
                  {folder && node.priority === null ? (
                    <option value="" disabled>
                      —
                    </option>
                  ) : null}
                  {PRIORITIES.map((priority) => (
                    <option key={priority} value={priority}>
                      {t(`detail.priorities.${priority}`)}
                    </option>
                  ))}
                </select>
              ) : (
                <span
                  className="w-16 shrink-0 text-right text-muted-foreground"
                  aria-hidden
                >
                  —
                </span>
              )}
            </div>
          );
        })}
      </div>
      <div
        aria-live="polite"
        className="border-t border-border px-2 py-1 text-sm text-muted-foreground"
      >
        {t("detail.files.summary", {
          selected: selectedLeaves.length,
          total: all.length,
          wanted: formatBytes(wantedBytes, locale),
          size: formatBytes(totalBytes, locale),
        })}
        {" · "}
        <span style={shortfall ? { color: "var(--error)" } : undefined}>
          {t("detail.files.free", {
            free: freeBytes == null ? "—" : formatBytes(freeBytes, locale),
          })}
        </span>
      </div>
    </div>
  );
}
