import { useMemo, useState, type ReactNode } from "react";
import { NavLink } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { ChevronDown } from "lucide-react";
import {
  selectCategoryCounts,
  selectFilterCounts,
  selectTagCounts,
  useTasks,
} from "../../store/useTasks";

/** Doc 09 §2.4: the fixed DOWNLOAD nodes, their filters and routes. */
export const DOWNLOAD_NODES = [
  { filter: "all", to: "/" },
  { filter: "downloading", to: "/tasks/downloading" },
  { filter: "completed", to: "/tasks/completed" },
  { filter: "active", to: "/tasks/active" },
  { filter: "inactive", to: "/tasks/inactive" },
  { filter: "stopped", to: "/tasks/stopped" },
  { filter: "error", to: "/tasks/error" },
] as const;

const dimmedOpacity = 0.45;

function NodeLink({
  to,
  label,
  count,
  end,
  dim,
}: {
  to: string;
  label: string;
  count?: number;
  end?: boolean;
  dim?: boolean;
}) {
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        `flex items-center gap-1.5 rounded px-2 py-1 ${isActive ? "bg-muted font-medium" : "hover:bg-muted/50"}`
      }
      style={dim ? { opacity: dimmedOpacity } : undefined}
    >
      <span className="truncate">{label}</span>
      {typeof count === "number" && (
        <span className="count ml-auto shrink-0 text-muted-foreground tabular-nums">
          {count}
        </span>
      )}
    </NavLink>
  );
}

/** A nav landmark with an sr-only heading, per the markup contract of doc 09 §2.4. */
function Group({
  id,
  label,
  children,
}: {
  id: string;
  label: string;
  children: ReactNode;
}) {
  return (
    <nav aria-labelledby={id}>
      <h2 id={id} className="sr-only">
        {label}
      </h2>
      <ul className="flex flex-col gap-0.5">{children}</ul>
    </nav>
  );
}

/** CATEGORIES and TAGS collapse; the toggle is their only visible group header. */
function CollapsibleGroup({
  id,
  label,
  children,
}: {
  id: string;
  label: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(true);
  return (
    <nav aria-labelledby={id}>
      <h2 id={id} className="sr-only">
        {label}
      </h2>
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-list`}
        onClick={() => setOpen((value) => !value)}
        className="flex w-full items-center gap-1 rounded px-2 py-1 text-xs font-medium text-muted-foreground hover:bg-muted/50"
      >
        <ChevronDown
          size={14}
          aria-hidden="true"
          className={open ? undefined : "-rotate-90"}
        />
        {label}
      </button>
      {open && (
        <ul id={`${id}-list`} className="mt-0.5 flex flex-col gap-0.5">
          {children}
        </ul>
      )}
    </nav>
  );
}

export function Sidebar() {
  const { t, i18n } = useTranslation();
  const state = useTasks();
  // The count selectors read state.tasks only; recompute when the map identity changes.
  const tasks = state.tasks;
  const filterCounts = useMemo(() => selectFilterCounts(state), [tasks]);
  const categoryCounts = useMemo(() => selectCategoryCounts(state), [tasks]);
  const tagCounts = useMemo(() => selectTagCounts(state), [tasks]);
  const collator = useMemo(
    () => new Intl.Collator(i18n.language),
    [i18n.language],
  );
  const categories = useMemo(
    () =>
      [...categoryCounts.entries()]
        .filter(
          (entry): entry is [string, number] => typeof entry[0] === "string",
        )
        .sort((a, b) => collator.compare(a[0], b[0])),
    [categoryCounts, collator],
  );
  const tags = useMemo(
    () => [...tagCounts.entries()].sort((a, b) => collator.compare(a[0], b[0])),
    [tagCounts, collator],
  );
  const untagged = useMemo(
    () =>
      [...tasks.values()].filter((task) => (task.tags ?? []).length === 0)
        .length,
    [tasks],
  );

  return (
    <div className="flex flex-col gap-3 p-2 text-sm">
      <Group id="nav-download" label={t("sidebar.download")}>
        {DOWNLOAD_NODES.map((node) => (
          <li key={node.filter}>
            <NodeLink
              to={node.to}
              end={node.to === "/"}
              label={t(`sidebar.${node.filter}`)}
              count={filterCounts[node.filter]}
              dim={filterCounts[node.filter] === 0}
            />
          </li>
        ))}
      </Group>
      <CollapsibleGroup id="nav-categories" label={t("sidebar.categories")}>
        {categories.map(([name, count]) => (
          <li key={name}>
            <NodeLink
              to={`/tasks/category/${encodeURIComponent(name)}`}
              label={name}
              count={count}
            />
          </li>
        ))}
        <li>
          <NodeLink
            to="/tasks/category"
            end
            label={t("sidebar.uncategorised")}
            count={categoryCounts.get(null) ?? 0}
          />
        </li>
      </CollapsibleGroup>
      <CollapsibleGroup id="nav-tags" label={t("sidebar.tags")}>
        {tags.map(([name, count]) => (
          <li key={name}>
            <NodeLink
              to={`/tasks/tag/${encodeURIComponent(name)}`}
              label={name}
              count={count}
            />
          </li>
        ))}
        <li>
          <NodeLink
            to="/tasks/tag"
            end
            label={t("sidebar.untagged")}
            count={untagged}
          />
        </li>
      </CollapsibleGroup>
      <Group id="nav-search" label={t("sidebar.search")}>
        <li>
          <NodeLink to="/search" label={t("sidebar.searchResults")} />
        </li>
        <li>
          <NodeLink to="/search/saved" label={t("sidebar.savedSearches")} />
        </li>
      </Group>
      <Group id="nav-rss" label={t("sidebar.rss")}>
        <li>
          <NodeLink to="/rss/feeds" label={t("sidebar.feeds")} />
        </li>
        <li>
          <NodeLink to="/rss/rules" label={t("sidebar.rules")} />
        </li>
      </Group>
      <hr aria-hidden="true" />
      <ul className="flex flex-col gap-0.5">
        <li>
          <NodeLink to="/settings/general" label={t("sidebar.settings")} />
        </li>
        <li>
          <NodeLink to="/logs" label={t("sidebar.logs")} />
        </li>
      </ul>
    </div>
  );
}
