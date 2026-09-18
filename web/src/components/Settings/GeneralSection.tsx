import { useCallback, useEffect, useRef, useState, type JSX } from "react";
import { useTranslation } from "react-i18next";
import { initI18n } from "../../i18n";
import { applyTheme, storeTheme, type ThemeChoice } from "../../lib/theme";
import settingsStrings from "../../locales/en/settings.json";
import type { SidebarFilter } from "../../store/useTasks";
import { useUiPrefs, type UiPrefs } from "../../store/useUiPrefs";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";
import { useSettingsDirty } from "./SettingsScreen";

initI18n().addResourceBundle("en", "settings", settingsStrings);

export type DoubleClickAction = "start-stop" | "open-detail" | "none";

/** The members doc 09 §9's General row adds to the preference document.
 *  They are unknown members (doc 05 §11.4): the store's typed surface does not
 *  declare them, so reads cast to this shape — the same access pattern
 *  AddTaskDialog already uses for `rememberLastDestination`. Persistence here
 *  landed ahead of the consumers: `startupFilter`, `confirmOnDelete` and the
 *  double-click actions stay write-only until the surfaces that own them —
 *  startup routing, the remove flow and the grid row — read them. When that
 *  wiring lands, `confirmOnDelete` may only skip the plain remove dialog; a
 *  delete-with-files request stays confirmed. */
interface GeneralExtraPrefs {
  startupFilter?: SidebarFilter;
  rememberLastDestination?: boolean;
  confirmOnDelete?: boolean;
  doubleClickDownloading?: DoubleClickAction;
  doubleClickCompleted?: DoubleClickAction;
}

interface GeneralValues {
  theme: ThemeChoice;
  density: UiPrefs["grid"]["density"];
  startupFilter: SidebarFilter;
  rememberLastDestination: boolean;
  confirmOnDelete: boolean;
  doubleClickDownloading: DoubleClickAction;
  doubleClickCompleted: DoubleClickAction;
}

const FIELD_KEYS: (keyof GeneralValues)[] = [
  "theme",
  "density",
  "startupFilter",
  "rememberLastDestination",
  "confirmOnDelete",
  "doubleClickDownloading",
  "doubleClickCompleted",
];

const DEFAULT_STARTUP_FILTER: SidebarFilter = "all";
const DEFAULT_DOUBLE_CLICK: DoubleClickAction = "open-detail";

const selectClass =
  "h-8 w-56 rounded-lg border border-input bg-transparent px-2 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 outline-none";

/** Every control writes through useUiPrefs.patch; nothing here calls the settings API. */
export function GeneralSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const theme = useUiPrefs((s) => s.theme);
  const density = useUiPrefs((s) => s.grid.density);
  const startupFilter = useUiPrefs(
    (s) => (s as GeneralExtraPrefs).startupFilter ?? DEFAULT_STARTUP_FILTER,
  );
  const rememberLastDestination = useUiPrefs(
    (s) => (s as GeneralExtraPrefs).rememberLastDestination ?? false,
  );
  const confirmOnDelete = useUiPrefs(
    (s) => (s as GeneralExtraPrefs).confirmOnDelete ?? true,
  );
  const doubleClickDownloading = useUiPrefs(
    (s) =>
      (s as GeneralExtraPrefs).doubleClickDownloading ?? DEFAULT_DOUBLE_CLICK,
  );
  const doubleClickCompleted = useUiPrefs(
    (s) =>
      (s as GeneralExtraPrefs).doubleClickCompleted ?? DEFAULT_DOUBLE_CLICK,
  );

  const values: GeneralValues = {
    theme,
    density,
    startupFilter,
    rememberLastDestination,
    confirmOnDelete,
    doubleClickDownloading,
    doubleClickCompleted,
  };
  const [baseline, setBaseline] = useState<GeneralValues>(() => values);
  const dirtyCount = FIELD_KEYS.filter(
    (key) => values[key] !== baseline[key],
  ).length;

  const valuesRef = useRef(values);
  valuesRef.current = values;
  const baselineRef = useRef(baseline);
  baselineRef.current = baseline;

  const apply = useCallback((next: Partial<GeneralValues>) => {
    const state = useUiPrefs.getState();
    const verbatim: Record<string, unknown> = {};
    if (next.startupFilter !== undefined)
      verbatim.startupFilter = next.startupFilter;
    if (next.rememberLastDestination !== undefined)
      verbatim.rememberLastDestination = next.rememberLastDestination;
    if (next.confirmOnDelete !== undefined)
      verbatim.confirmOnDelete = next.confirmOnDelete;
    if (next.doubleClickDownloading !== undefined)
      verbatim.doubleClickDownloading = next.doubleClickDownloading;
    if (next.doubleClickCompleted !== undefined)
      verbatim.doubleClickCompleted = next.doubleClickCompleted;
    // Unknown members go through patch alone: the debounced writer now
    // serializes every non-function state member, so they reach the document
    // verbatim (doc 05 §11.4).
    if (Object.keys(verbatim).length > 0)
      state.patch(verbatim as Omit<Partial<UiPrefs>, "version">);

    const known: Omit<Partial<UiPrefs>, "version"> = {};
    if (next.density !== undefined)
      known.grid = { ...state.grid, density: next.density };
    if (Object.keys(known).length > 0) state.patch(known);

    if (next.theme !== undefined) {
      // lib/theme.ts owns the class and the stored member, per T045.
      storeTheme(next.theme);
      applyTheme(next.theme);
    }
  }, []);

  const save = useCallback(() => setBaseline(valuesRef.current), []);
  const revert = useCallback(() => apply(baselineRef.current), [apply]);

  useEffect(() => {
    useSettingsDirty.setState((prev) => {
      if (dirtyCount === 0)
        return prev.report === null ? prev : { report: null };
      if (
        prev.report?.count === dirtyCount &&
        prev.report.section === "general"
      )
        return prev;
      return {
        report: { section: "general", count: dirtyCount, save, revert },
      };
    });
  }, [dirtyCount, save, revert]);
  useEffect(() => () => useSettingsDirty.setState({ report: null }), []);

  return (
    <div className="flex max-w-xl flex-col gap-5">
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="general-theme">{t("general.theme")}</Label>
        <select
          id="general-theme"
          className={selectClass}
          value={theme}
          onChange={(event) =>
            apply({ theme: event.target.value as ThemeChoice })
          }
        >
          <option value="system">{ct("theme.system")}</option>
          <option value="light">{ct("theme.light")}</option>
          <option value="dark">{ct("theme.dark")}</option>
        </select>
      </div>
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="general-density">{t("general.density")}</Label>
        <select
          id="general-density"
          className={selectClass}
          value={density}
          onChange={(event) =>
            apply({
              density: event.target.value as GeneralValues["density"],
            })
          }
        >
          <option value="comfortable">{t("general.densityComfortable")}</option>
          <option value="compact">{t("general.densityCompact")}</option>
        </select>
      </div>
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="general-startup-filter">
          {t("general.startupFilter")}
        </Label>
        <select
          id="general-startup-filter"
          className={selectClass}
          value={startupFilter}
          onChange={(event) =>
            apply({ startupFilter: event.target.value as SidebarFilter })
          }
        >
          {(
            [
              "all",
              "downloading",
              "completed",
              "active",
              "inactive",
              "stopped",
              "error",
            ] as const
          ).map((filter) => (
            <option key={filter} value={filter}>
              {ct(`shell.sidebar.${filter}`)}
            </option>
          ))}
        </select>
      </div>
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="general-remember-destination">
          {t("general.rememberLastDestination")}
        </Label>
        <Checkbox
          id="general-remember-destination"
          checked={rememberLastDestination}
          onCheckedChange={(checked) =>
            apply({ rememberLastDestination: checked === true })
          }
        />
      </div>
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="general-confirm-delete">
          {t("general.confirmOnDelete")}
        </Label>
        <Checkbox
          id="general-confirm-delete"
          checked={confirmOnDelete}
          onCheckedChange={(checked) =>
            apply({ confirmOnDelete: checked === true })
          }
        />
      </div>
      <fieldset className="flex flex-col gap-3">
        <legend className="text-sm font-medium">
          {t("general.doubleClick")}
        </legend>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="general-double-click-downloading">
            {t("general.doubleClickDownloading")}
          </Label>
          <select
            id="general-double-click-downloading"
            className={selectClass}
            value={doubleClickDownloading}
            onChange={(event) =>
              apply({
                doubleClickDownloading: event.target.value as DoubleClickAction,
              })
            }
          >
            <option value="start-stop">
              {t("general.doubleClickStartStop")}
            </option>
            <option value="open-detail">
              {t("general.doubleClickOpenDetail")}
            </option>
            <option value="none">{t("general.doubleClickNone")}</option>
          </select>
        </div>
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="general-double-click-completed">
            {t("general.doubleClickCompleted")}
          </Label>
          <select
            id="general-double-click-completed"
            className={selectClass}
            value={doubleClickCompleted}
            onChange={(event) =>
              apply({
                doubleClickCompleted: event.target.value as DoubleClickAction,
              })
            }
          >
            <option value="start-stop">
              {t("general.doubleClickStartStop")}
            </option>
            <option value="open-detail">
              {t("general.doubleClickOpenDetail")}
            </option>
            <option value="none">{t("general.doubleClickNone")}</option>
          </select>
        </div>
      </fieldset>
    </div>
  );
}
