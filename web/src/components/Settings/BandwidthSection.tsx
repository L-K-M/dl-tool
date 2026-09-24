import { useCallback, useEffect, useRef, useState, type JSX } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import {
  CELL_FILL,
  clearAll,
  copyMondayToWeekdays,
  fillAll,
  invert,
  ScheduleGrid,
  STATE_NAME,
  type Brush,
  type Cells,
} from "./ScheduleGrid";
import { useSettingsDirty } from "./SettingsScreen";

initI18n().addResourceBundle("en", "settings", settingsStrings);

/** The four rate-limit keys of GET /settings, doc 11 §5. Bytes per second everywhere; 0 = unlimited.
 *  No KB/s value exists on this screen, in either direction. */
export interface BandwidthSettings {
  download_rate_limit: number;
  upload_rate_limit: number;
  alt_download_rate_limit: number;
  alt_upload_rate_limit: number;
}

/** GET /settings/schedule, doc 05 §11.2. timezone and active_mode are read-only: they are rendered
 *  and echoed back unchanged, and the server ignores whatever a client sends. */
export interface ScheduleBody {
  enabled: boolean;
  cells: number[];
  timezone: string;
  active_mode: "no_download" | "default" | "alternative";
}

/** The radio is a rendering of ScheduleBody.enabled and of nothing else. The wire has no third value.
 *  'immediately'       → enabled:false — the global pair is in force at all times and the grid is inert.
 *  'advanced_schedule' → enabled:true  — the cell in force selects the global or the alternative pair. */
export type ScheduleChoice = "immediately" | "advanced_schedule";

const SETTINGS_KEY = ["settings"] as const;
const SCHEDULE_KEY = ["settings-schedule"] as const;
const BRUSHES: Brush[] = [0, 1, 2];

// The plan's canonical repository documentation; not a runtime setting.
const PRECEDENCE_URL =
  "https://github.com/L-K-M/dl-tool/blob/main/docs/06-download-engines.md#10-bandwidth-precedence-and-fan-out";

/** The four editable limits in the order the screen renders them: the global
 *  pair first, then the alternative pair under its own heading. */
const LIMIT_FIELDS = [
  "download",
  "upload",
  "altDownload",
  "altUpload",
] as const;
type LimitField = (typeof LIMIT_FIELDS)[number];
const LIMIT_KEY: Record<LimitField, keyof BandwidthSettings> = {
  download: "download_rate_limit",
  upload: "upload_rate_limit",
  altDownload: "alt_download_rate_limit",
  altUpload: "alt_upload_rate_limit",
};

function problemDetail(
  error: { detail?: string; title?: string; type?: string } | undefined,
): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

/** The editable state, held as the inputs' own text so an in-progress edit
 *  round-trips without reformatting; the cells stay the wire encoding. */
interface FormState {
  download: string;
  upload: string;
  altDownload: string;
  altUpload: string;
  enabled: boolean;
  cells: Cells;
}

/** The two read-only members of the schedule body, kept to render and echo back. */
interface ScheduleMeta {
  timezone: string;
  active_mode: ScheduleBody["active_mode"];
}

function seedForm(
  settings: BandwidthSettings,
  schedule: ScheduleBody,
): FormState {
  return {
    download: String(settings.download_rate_limit),
    upload: String(settings.upload_rate_limit),
    altDownload: String(settings.alt_download_rate_limit),
    altUpload: String(settings.alt_upload_rate_limit),
    enabled: schedule.enabled,
    cells: schedule.cells.slice() as Cells,
  };
}

function cellsEqual(a: Cells, b: Cells): boolean {
  return a.length === b.length && a.every((cell, i) => cell === b[i]);
}

export function BandwidthSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();

  const settings = useQuery({
    queryKey: SETTINGS_KEY,
    queryFn: async (): Promise<BandwidthSettings> => {
      const { data, error } = await api.GET("/settings");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return {
        download_rate_limit: data.download_rate_limit,
        upload_rate_limit: data.upload_rate_limit,
        alt_download_rate_limit: data.alt_download_rate_limit,
        alt_upload_rate_limit: data.alt_upload_rate_limit,
      };
    },
    retry: false,
  });
  const schedule = useQuery({
    queryKey: SCHEDULE_KEY,
    queryFn: async (): Promise<ScheduleBody> => {
      const { data, error } = await api.GET("/settings/schedule");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return {
        enabled: data.enabled,
        cells: data.cells,
        timezone: data.timezone ?? "",
        active_mode: data.active_mode ?? "default",
      };
    },
    retry: false,
  });

  const ready = settings.data !== undefined && schedule.data !== undefined;

  const [form, setForm] = useState<FormState | null>(null);
  const [baseline, setBaseline] = useState<FormState | null>(null);
  const [meta, setMeta] = useState<ScheduleMeta | null>(null);
  const [brush, setBrush] = useState<Brush>(1);
  const [limitErrors, setLimitErrors] = useState<
    Partial<Record<LimitField, string>>
  >({});
  const [saveError, setSaveError] = useState<string | null>(null);
  const savingRef = useRef(false);

  const formRef = useRef(form);
  formRef.current = form;
  const baselineRef = useRef(baseline);
  baselineRef.current = baseline;
  const metaRef = useRef(meta);
  metaRef.current = meta;

  // The seed runs once, on the first render with both responses; a later
  // refetch refreshes the queries, not the in-progress form.
  useEffect(() => {
    if (!ready || form !== null) return;
    const seeded = seedForm(
      settings.data as BandwidthSettings,
      schedule.data as ScheduleBody,
    );
    setForm(seeded);
    setBaseline(seeded);
    setMeta({
      timezone: (schedule.data as ScheduleBody).timezone,
      active_mode: (schedule.data as ScheduleBody).active_mode,
    });
  }, [ready, form, settings.data, schedule.data]);

  const dirtyCount =
    form === null || baseline === null
      ? 0
      : [
          form.download !== baseline.download,
          form.upload !== baseline.upload,
          form.altDownload !== baseline.altDownload,
          form.altUpload !== baseline.altUpload,
          form.enabled !== baseline.enabled,
          !cellsEqual(form.cells, baseline.cells),
        ].filter(Boolean).length;

  /** One PATCH /settings carrying only the changed limit keys, then one
   *  PUT /settings/schedule with all 168 cells and enabled. A rejected PATCH
   *  ends the save with the form still dirty; a rejected PUT keeps the grid
   *  and the radio dirty while the limits that landed become the baseline. */
  const save = useCallback(() => {
    const current = formRef.current;
    const base = baselineRef.current;
    const scheduleMeta = metaRef.current;
    if (
      current === null ||
      base === null ||
      scheduleMeta === null ||
      savingRef.current
    )
      return;
    savingRef.current = true;
    void (async () => {
      try {
        const patch: Partial<BandwidthSettings> = {};
        const fieldErrors: Partial<Record<LimitField, string>> = {};
        for (const field of LIMIT_FIELDS) {
          if (current[field] === base[field]) continue;
          const value = Number(current[field]);
          // Anything but a non-negative whole number is a guaranteed 422, so
          // it fails on the field now instead of shipping a doomed PATCH.
          if (
            current[field].trim() === "" ||
            !Number.isInteger(value) ||
            value < 0
          ) {
            fieldErrors[field] = t("bandwidth.invalidLimit");
            continue;
          }
          patch[LIMIT_KEY[field]] = value;
        }
        if (Object.keys(fieldErrors).length > 0) {
          setLimitErrors(fieldErrors);
          return;
        }
        setLimitErrors({});

        let landed = base;
        if (Object.keys(patch).length > 0) {
          const { error } = await api.PATCH("/settings", { body: patch });
          if (error !== undefined) {
            setSaveError(
              t("bandwidth.saveFailed", {
                detail: problemDetail(error) ?? ct("shell.networkError"),
              }),
            );
            return;
          }
          landed = { ...current, enabled: base.enabled, cells: base.cells };
          await queryClient.invalidateQueries({ queryKey: SETTINGS_KEY });
        }

        const { error } = await api.PUT("/settings/schedule", {
          body: {
            enabled: current.enabled,
            cells: current.cells,
            timezone: scheduleMeta.timezone,
            active_mode: scheduleMeta.active_mode,
          },
        });
        if (error !== undefined) {
          setSaveError(
            t("bandwidth.saveFailed", {
              detail: problemDetail(error) ?? ct("shell.networkError"),
            }),
          );
          setBaseline(landed);
          return;
        }
        setBaseline(current);
        setSaveError(null);
        await queryClient.invalidateQueries({ queryKey: SCHEDULE_KEY });
      } finally {
        savingRef.current = false;
      }
    })();
  }, [queryClient, t, ct]);

  const revert = useCallback(() => {
    setForm(baselineRef.current);
    setLimitErrors({});
    setSaveError(null);
  }, []);

  useEffect(() => {
    useSettingsDirty.setState((prev) => {
      if (dirtyCount === 0)
        return prev.report === null ? prev : { report: null };
      if (
        prev.report?.count === dirtyCount &&
        prev.report.section === "bandwidth"
      )
        return prev;
      return {
        report: { section: "bandwidth", count: dirtyCount, save, revert },
      };
    });
  }, [dirtyCount, save, revert]);
  useEffect(() => () => useSettingsDirty.setState({ report: null }), []);

  const failed = [settings, schedule].some(
    (query) => query.isError && query.data === undefined,
  );
  if (failed)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("bandwidth.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            void settings.refetch();
            void schedule.refetch();
          }}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );
  if (form === null) return <></>;

  const limitInput = (
    field: LimitField,
    id: string,
    label: string,
  ): JSX.Element => (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex items-center gap-1.5">
        <Input
          id={id}
          type="number"
          min={0}
          step={1}
          className="w-32"
          value={form[field]}
          aria-invalid={limitErrors[field] !== undefined || undefined}
          aria-describedby={
            limitErrors[field] !== undefined ? `${id}-error` : undefined
          }
          onChange={(event) => {
            setForm({ ...form, [field]: event.target.value });
            setLimitErrors((prev) => ({ ...prev, [field]: undefined }));
          }}
        />
        <span className="text-xs text-muted-foreground">
          {t("bandwidth.bytesPerSecond")}
        </span>
      </div>
      {limitErrors[field] !== undefined && (
        <p id={`${id}-error`} role="alert" className="text-xs text-destructive">
          {limitErrors[field]}
        </p>
      )}
    </div>
  );

  return (
    <div className="flex flex-col gap-5">
      {saveError !== null && (
        <p role="alert" className="text-sm text-destructive">
          {saveError}
        </p>
      )}

      <div className="flex flex-wrap items-end gap-4">
        {limitInput("download", "bw-download", t("bandwidth.downloadLimit"))}
        {limitInput("upload", "bw-upload", t("bandwidth.uploadLimit"))}
        <p className="pb-1.5 text-xs text-muted-foreground">
          ({t("bandwidth.unlimitedHint")})
        </p>
      </div>

      <div
        role="radiogroup"
        aria-label={t("bandwidth.mode")}
        className="flex items-center gap-4"
      >
        <label className="flex items-center gap-1.5 text-sm">
          <input
            type="radio"
            name="bandwidth-mode"
            checked={!form.enabled}
            onChange={() => setForm({ ...form, enabled: false })}
          />
          {t("bandwidth.immediately")}
        </label>
        <label className="flex items-center gap-1.5 text-sm">
          <input
            type="radio"
            name="bandwidth-mode"
            checked={form.enabled}
            onChange={() => setForm({ ...form, enabled: true })}
          />
          {t("bandwidth.advancedSchedule")}
        </label>
      </div>

      <div
        role="group"
        aria-label={t("bandwidth.altHeading")}
        className="flex flex-col gap-2"
      >
        <h2 className="text-sm font-medium">{t("bandwidth.altHeading")}</h2>
        <div className="flex flex-wrap items-end gap-4">
          {limitInput(
            "altDownload",
            "bw-alt-download",
            t("bandwidth.altDownloadLimit"),
          )}
          {limitInput(
            "altUpload",
            "bw-alt-upload",
            t("bandwidth.altUploadLimit"),
          )}
          <p className="pb-1.5 text-xs text-muted-foreground">
            ({t("bandwidth.unlimitedHint")})
          </p>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-4">
        <div
          role="group"
          aria-label={t("bandwidth.brush")}
          className="flex items-center gap-1.5"
        >
          <span className="text-sm">{t("bandwidth.brush")}:</span>
          {BRUSHES.map((b) => (
            <Button
              key={b}
              type="button"
              variant={brush === b ? "default" : "outline"}
              size="sm"
              aria-pressed={brush === b}
              onClick={() => setBrush(b)}
            >
              {t(STATE_NAME[b])}
            </Button>
          ))}
        </div>
        <p className="text-sm text-muted-foreground">
          {t("bandwidth.timezone", { zone: meta?.timezone || "—" })}
        </p>
      </div>

      <div className="overflow-x-auto">
        <ScheduleGrid
          cells={form.cells}
          brush={brush}
          disabled={!form.enabled}
          onChange={(cells) => setForm({ ...form, cells })}
        />
      </div>

      <div
        role="group"
        aria-label={t("bandwidth.legend")}
        className="flex flex-wrap items-center gap-x-4 gap-y-1"
      >
        <span className="text-sm font-medium">{t("bandwidth.legend")}</span>
        {BRUSHES.map((b) => (
          <span key={b} className="flex items-center gap-1.5 text-sm">
            <span
              aria-hidden="true"
              className="inline-block size-4 rounded-sm border border-border"
              style={CELL_FILL[b]}
            />
            {t(STATE_NAME[b])}
          </span>
        ))}
      </div>
      <p className="-mt-3 text-xs text-muted-foreground">
        {t("bandwidth.pauseNote")}{" "}
        <a
          href={PRECEDENCE_URL}
          target="_blank"
          rel="noreferrer"
          className="underline underline-offset-2"
        >
          {t("bandwidth.pauseNoteLink")}
        </a>
      </p>

      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!form.enabled}
          onClick={() =>
            setForm({ ...form, cells: fillAll(form.cells, brush) })
          }
        >
          {t("bandwidth.fillAll", { brush: t(STATE_NAME[brush]) })}
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!form.enabled}
          onClick={() => setForm({ ...form, cells: clearAll(form.cells) })}
        >
          {t("bandwidth.clear")}
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!form.enabled}
          onClick={() =>
            setForm({ ...form, cells: copyMondayToWeekdays(form.cells) })
          }
        >
          {t("bandwidth.copyMonday")}
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={!form.enabled}
          onClick={() => setForm({ ...form, cells: invert(form.cells) })}
        >
          {t("bandwidth.invert")}
        </Button>
      </div>
    </div>
  );
}
