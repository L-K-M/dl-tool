import { useLayoutEffect, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { Link, Navigate, useParams } from "react-router-dom";
import { create } from "zustand";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { BandwidthSection } from "./BandwidthSection";
import { ConnectionSection } from "./ConnectionSection";
import { GeneralSection } from "./GeneralSection";
import { IndexersSection } from "./IndexersSection";
import { RssSection } from "./RssSection";

initI18n().addResourceBundle("en", "settings", settingsStrings);

export const SECTIONS = [
  "general",
  "connection",
  "bandwidth",
  "bittorrent",
  "downloads",
  "rss",
  "indexers",
  "account",
  "notifications",
  "advanced",
] as const;
export type Section = (typeof SECTIONS)[number];

/** The form each implemented section renders. SECTION_FORMS is the single
 *  source of truth: IMPLEMENTED derives from its keys, and ARRIVAL must cover
 *  every section that has no form. */
const SECTION_FORMS = {
  general: GeneralSection,
  connection: ConnectionSection,
  bandwidth: BandwidthSection,
  rss: RssSection,
  indexers: IndexersSection,
} satisfies Partial<Record<Section, () => JSX.Element>>;

/** Sections whose endpoints exist at M3. Every other section renders the one-line note
 *  "This section arrives with <milestone>." and is never a broken form. */
export const IMPLEMENTED: Section[] = Object.keys(SECTION_FORMS) as Section[];

const KNOWN_SECTIONS: ReadonlySet<string> = new Set(SECTIONS);

/** The milestone that ships each section's backing endpoints; advanced lands in
 *  M7, the rest in M6 (docs/tasks/00-task-index.md). The record
 *  is exhaustive: adding a section without an entry is a type error, not a
 *  silently wrong milestone. */
export const ARRIVAL: Record<
  Exclude<Section, keyof typeof SECTION_FORMS>,
  string
> = {
  bittorrent: "M6",
  downloads: "M6",
  account: "M6",
  notifications: "M6",
  advanced: "M7",
};

/** What a section form reports to the shell's Save / Revert bar. */
export interface DirtyReport {
  /** Owning section; the shell drops a report published by any other. */
  section: Section;
  count: number;
  save: () => void;
  revert: () => void;
}

/** The mounted section publishes its dirty state; SettingsScreen renders the
 *  bar. Null while clean or while no reporting section is mounted. */
export const useSettingsDirty = create<{ report: DirtyReport | null }>(() => ({
  report: null,
}));

export function SettingsScreen(): JSX.Element {
  const { t } = useTranslation("settings");
  const params = useParams();
  // Doc 09 §2.1 still routes the account section as `users`; ADR-0019 dropped
  // the multi-user model and the section became "Account & Auth". Keep the
  // documented path reachable as a render alias rather than redirecting.
  const requested = params.section ?? "";
  const section: Section = (
    requested === "users" ? "account" : requested
  ) as Section;
  // The selector is authoritative: a report owned by another section never
  // renders, regardless of which effect flavor a form publishes from.
  const report = useSettingsDirty((state) =>
    state.report?.section === section ? state.report : null,
  );

  // Any report present when the section mounts or changes is stale — its
  // save/revert closures belong to a previous form instance (including a
  // save that resolved after navigation). This is a layout effect so the
  // clear runs before a child's passive publish (passive effects run
  // child-first; section forms MUST publish from useEffect, never
  // useLayoutEffect). The selector only filters cross-section reports; a
  // fresh same-section report published from useLayoutEffect would still be
  // cleared here, so this ordering remains load-bearing for display.
  useLayoutEffect(() => {
    useSettingsDirty.setState({ report: null });
    return () => useSettingsDirty.setState({ report: null });
  }, [section]);

  if (!KNOWN_SECTIONS.has(section))
    return <Navigate to="/settings/general" replace />;

  const Form = (SECTION_FORMS as Partial<Record<Section, () => JSX.Element>>)[
    section
  ];

  return (
    <div className="flex h-full min-h-0">
      <nav
        aria-label={t("nav.label")}
        className="w-48 shrink-0 overflow-y-auto border-e border-border px-2 py-3"
      >
        <ul className="flex flex-col gap-0.5">
          {SECTIONS.map((name) => {
            const current = name === section;
            return (
              <li key={name}>
                <Link
                  to={`/settings/${name}`}
                  aria-current={current ? "page" : undefined}
                  className={`block rounded px-2 py-1.5 text-sm ${
                    current
                      ? "bg-accent font-medium text-accent-foreground"
                      : "hover:bg-muted"
                  }`}
                >
                  {t(`nav.${name}`)}
                </Link>
              </li>
            );
          })}
        </ul>
      </nav>
      <div className="relative min-w-0 flex-1 overflow-y-auto">
        <h1 className="sticky top-0 z-10 border-b border-border bg-background px-6 py-3 text-lg font-semibold">
          {t(`nav.${section}`)}
        </h1>
        <div className="px-6 py-4">
          {Form !== undefined ? (
            <Form />
          ) : (
            <p className="text-sm text-muted-foreground">
              {t("arrives", {
                milestone: ARRIVAL[section as keyof typeof ARRIVAL],
              })}
            </p>
          )}
        </div>
        {/* Always mounted so the count change announces; visually hidden while clean. */}
        <p aria-live="polite" className="sr-only">
          {report === null ? "" : t("dirty.unsaved", { count: report.count })}
        </p>
        {report !== null && (
          <div className="sticky bottom-0 z-10 flex items-center gap-2 border-t border-border bg-background px-6 py-2">
            <p className="me-auto text-sm">
              {t("dirty.unsaved", { count: report.count })}
            </p>
            <Button variant="outline" size="sm" onClick={report.revert}>
              {t("dirty.revert")}
            </Button>
            <Button size="sm" onClick={report.save}>
              {t("dirty.save")}
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}
