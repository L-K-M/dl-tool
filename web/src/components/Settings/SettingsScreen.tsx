import { type JSX } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, NavLink, useParams } from "react-router-dom";
import { create } from "zustand";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { ConnectionSection } from "./ConnectionSection";
import { GeneralSection } from "./GeneralSection";

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

/** Sections whose endpoints exist at M3. Every other section renders the one-line note
 *  "This section arrives with <milestone>." and is never a broken form. */
export const IMPLEMENTED: Section[] = ["general", "connection"];

const KNOWN_SECTIONS: ReadonlySet<string> = new Set(SECTIONS);

/** The milestone that ships each section's backing endpoints; indexers land in
 *  M4, advanced in M7, the rest in M6 (docs/tasks/00-task-index.md). */
const ARRIVAL: Partial<Record<Section, string>> = {
  indexers: "M4",
  bandwidth: "M6",
  bittorrent: "M6",
  downloads: "M6",
  rss: "M6",
  account: "M6",
  notifications: "M6",
  advanced: "M7",
};

/** What a section form reports to the shell's Save / Revert bar. */
export interface DirtyReport {
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
  const report = useSettingsDirty((state) => state.report);
  // Doc 09 §2.1 still routes the account section as `users`; ADR-0019 dropped
  // the multi-user model and the section became "Account & Auth". Keep the
  // documented path reachable as a render alias rather than redirecting.
  const requested = params.section ?? "";
  const section: Section = (
    requested === "users" ? "account" : requested
  ) as Section;
  if (!KNOWN_SECTIONS.has(section))
    return <Navigate to="/settings/general" replace />;

  return (
    <div className="flex h-full min-h-0">
      <nav
        aria-label={t("nav.label")}
        className="w-48 shrink-0 overflow-y-auto border-e border-border px-2 py-3"
      >
        <ul className="flex flex-col gap-0.5">
          {SECTIONS.map((name) => (
            <li key={name}>
              <NavLink
                to={`/settings/${name}`}
                className="block rounded px-2 py-1.5 text-sm hover:bg-muted aria-[current=page]:bg-accent aria-[current=page]:font-medium aria-[current=page]:text-accent-foreground"
              >
                {t(`nav.${name}`)}
              </NavLink>
            </li>
          ))}
        </ul>
      </nav>
      <div className="relative min-w-0 flex-1 overflow-y-auto">
        <h1 className="sticky top-0 z-10 border-b border-border bg-background px-6 py-3 text-lg font-semibold">
          {t(`nav.${section}`)}
        </h1>
        <div className="px-6 py-4">
          {section === "general" ? (
            <GeneralSection />
          ) : section === "connection" ? (
            <ConnectionSection />
          ) : (
            <p className="text-sm text-muted-foreground">
              {t("arrives", { milestone: ARRIVAL[section] ?? "M7" })}
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
