import { useState, type JSX, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { NavLink } from "react-router-dom";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useShallow } from "zustand/react/shallow";
import {
  selectCategoryCounts,
  selectFilterCounts,
  selectTagCounts,
  useTasks,
} from "../../store/useTasks";

export const DOWNLOAD_NODES = [
  { filter: "all", to: "/" },
  { filter: "downloading", to: "/tasks/downloading" },
  { filter: "completed", to: "/tasks/completed" },
  { filter: "active", to: "/tasks/active" },
  { filter: "inactive", to: "/tasks/inactive" },
  { filter: "stopped", to: "/tasks/stopped" },
  { filter: "error", to: "/tasks/error" },
] as const;

// Doc 09 section 2.4: a zero-count DOWNLOAD node dims, it never hides.
const zeroCountOpacity = 0.45;

function Count({ value }: { value: number }) {
  const { i18n } = useTranslation();
  return (
    <span className="count ml-auto rounded-full bg-muted px-1.5 text-xs text-muted-foreground">
      {new Intl.NumberFormat(i18n.language).format(value)}
    </span>
  );
}

function Node({
  to,
  label,
  count,
  dimmed,
  end,
}: {
  to: string;
  label: string;
  count?: number;
  dimmed?: boolean;
  /** Exact-match routes that are prefixes of their own children (category/tag fallbacks). */
  end?: boolean;
}) {
  return (
    <NavLink
      to={to}
      end={end}
      className="flex items-center gap-2 rounded px-2 py-1 text-sm hover:bg-muted"
      style={{ opacity: dimmed ? zeroCountOpacity : undefined }}
    >
      <span className="truncate">{label}</span>
      {count !== undefined && <Count value={count} />}
    </NavLink>
  );
}

function Group({
  id,
  label,
  collapsible,
  children,
}: {
  id: string;
  label: string;
  collapsible?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(true);
  return (
    <nav aria-labelledby={id} className="px-1 py-1">
      <h2 id={id} className="sr-only">
        {label}
      </h2>
      {collapsible ? (
        <button
          type="button"
          aria-expanded={open}
          onClick={() => setOpen((value) => !value)}
          className="flex w-full items-center gap-1 px-2 py-1 text-xs font-semibold uppercase text-muted-foreground"
        >
          {open ? (
            <ChevronDown size={12} aria-hidden="true" />
          ) : (
            <ChevronRight size={12} aria-hidden="true" />
          )}
          {label}
        </button>
      ) : (
        <div
          aria-hidden="true"
          className="px-2 py-1 text-xs font-semibold uppercase text-muted-foreground"
        >
          {label}
        </div>
      )}
      {(!collapsible || open) && children}
    </nav>
  );
}

function sortedEntries(counts: Map<string, number>): [string, number][] {
  return [...counts.entries()].sort(([left], [right]) =>
    left.localeCompare(right),
  );
}

export function Sidebar(): JSX.Element {
  const { t } = useTranslation();
  // Counts come from the SSE-fed store selectors, never one request per node.
  const filters = useTasks(useShallow(selectFilterCounts));
  const categories = useTasks(useShallow(selectCategoryCounts));
  const tags = useTasks(useShallow(selectTagCounts));
  const tasks = useTasks((state) => state.tasks);
  const namedCategories = sortedEntries(
    new Map(
      [...categories.entries()].filter(
        // != null also drops fixtures that omit the category field entirely.
        (entry): entry is [string, number] => entry[0] != null,
      ),
    ),
  );
  const uncategorised = [...tasks.values()].filter(
    (task) => !task.category,
  ).length;
  const untagged = [...tasks.values()].filter(
    (task) => !task.tags?.length,
  ).length;
  return (
    <div className="flex h-full flex-col gap-1 overflow-y-auto border-r border-border py-2">
      <Group id="nav-download" label={t("shell.sidebar.download")}>
        {DOWNLOAD_NODES.map(({ filter, to }) => (
          <Node
            key={filter}
            to={to}
            label={t(`shell.sidebar.${filter}`)}
            count={filters[filter]}
            dimmed={filters[filter] === 0}
          />
        ))}
      </Group>
      <Group
        id="nav-categories"
        label={t("shell.sidebar.categories")}
        collapsible
      >
        {namedCategories.map(([name, count]) => (
          <Node
            key={name}
            to={`/tasks/category/${encodeURIComponent(name)}`}
            label={name}
            count={count}
          />
        ))}
        <Node
          key="uncategorised"
          to="/tasks/category"
          end
          label={t("shell.sidebar.uncategorised")}
          count={uncategorised}
        />
      </Group>
      <Group id="nav-tags" label={t("shell.sidebar.tags")} collapsible>
        {sortedEntries(tags).map(([name, count]) => (
          <Node
            key={name}
            to={`/tasks/tag/${encodeURIComponent(name)}`}
            label={name}
            count={count}
          />
        ))}
        <Node
          key="untagged"
          to="/tasks/tag"
          end
          label={t("shell.sidebar.untagged")}
          count={untagged}
        />
      </Group>
      <Group id="nav-search" label={t("shell.sidebar.search")}>
        <Node to="/search" label={t("shell.sidebar.searchResults")} />
        <Node to="/search" label={t("shell.sidebar.savedSearches")} />
      </Group>
      <Group id="nav-rss" label={t("shell.sidebar.rss")}>
        <Node to="/rss/feeds" label={t("shell.sidebar.feeds")} />
        <Node to="/rss/rules" label={t("shell.sidebar.rules")} />
      </Group>
      <nav aria-label={t("regions.sidebar")} className="mt-auto px-1">
        <hr className="mx-2 my-1 border-border" />
        <Node to="/settings/general" label={t("shell.settings")} />
        <Node to="/logs" label={t("screens.logs")} />
      </nav>
    </div>
  );
}
