import { useCallback, useEffect, useRef, useState, type JSX } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { useSettingsDirty } from "./SettingsScreen";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type FeedDTO = components["schemas"]["FeedDTO"];
type RuleDTO = components["schemas"]["RuleDTO"];

/** The two RSS members of GET /settings, doc 11 §5. No other key on this screen exists. */
export interface RssSettings {
  rss_enabled: boolean;
  rss_interval_s: number; // seconds, server minimum 300
}

/** The fields of GET /feeds this section reads, doc 05 §10.1. */
export interface FeedCap {
  id: string;
  title: string | null;
  url: string;
  item_cap: number;
}

// The same keys FeedsScreen and RuleEditor query under, so the three views
// share one cache entry per resource.
const SETTINGS_KEY = ["settings"] as const;
const FEEDS_KEY = ["rss-feeds"] as const;
const RULES_KEY = ["rss-rules"] as const;

// The column default of feeds.item_cap (doc 05 §10.1); the cap input renders it
// disabled while no feed exists to read a live value from.
const DEFAULT_ITEM_CAP = 50;

// PATCH /settings answers 422 below this floor (doc 11 §5); the input's
// five-minute minimum is the same bound in the unit the user edits.
const MIN_INTERVAL_S = 300;

// Doc 08 §6.4 fixes these four patterns for v1; T069 compiles them into
// internal/rss/episode.go, so the screen renders them read-only, verbatim.
const SMART_PATTERNS = `s(\\d+)e(\\d+)                        # Format 1: s01e01
(\\d+)x(\\d+)                         # Format 2: 01x01
(\\d{4}[.\\-]\\d{1,2}[.\\-]\\d{1,2})     # Format 3: 2017.01.01
(\\d{1,2}[.\\-]\\d{1,2}[.\\-]\\d{4})     # Format 4: 01.01.2017`;

/** The value shown by "Maximum articles kept per feed".
 *  null  → no feed exists yet, the input renders the server default 50 and is disabled;
 *  'mixed' → feeds disagree, the input renders empty with the placeholder "Mixed"; typing a
 *            number and saving applies it to every feed. */
export function commonItemCap(feeds: FeedCap[]): number | "mixed" | null {
  const first = feeds[0]?.item_cap;
  if (first === undefined) return null;
  return feeds.every((feed) => feed.item_cap === first) ? first : "mixed";
}

/** Minutes are the unit the user edits; seconds are the unit the API stores.
 *  Round-trips: secondsToMinutes(1800) === 30, minutesToSeconds(30) === 1800. The input's min is 5
 *  because the server rejects rss_interval_s below 300 with 422. */
export function secondsToMinutes(s: number): number {
  return s / 60;
}
export function minutesToSeconds(m: number): number {
  return m * 60;
}

function problemDetail(
  error: { detail?: string; title?: string; type?: string } | undefined,
): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

/** The editable state, held as the inputs' own units — minutes and the literal
 *  input text — so an in-progress edit round-trips without reformatting. */
interface FormState {
  enabled: boolean;
  intervalMin: string;
  cap: string;
}

function seedForm(settings: RssSettings, feeds: FeedCap[]): FormState {
  const cap = commonItemCap(feeds);
  return {
    enabled: settings.rss_enabled,
    intervalMin: String(secondsToMinutes(settings.rss_interval_s)),
    cap:
      typeof cap === "number"
        ? String(cap)
        : cap === "mixed"
          ? ""
          : String(DEFAULT_ITEM_CAP),
  };
}

/** The cap fans out only when the input holds a real number that differs from
 *  the seeded display: a cleared input is not a value to apply to every feed. */
function capApplies(current: string, base: string, feedCount: number): boolean {
  return (
    feedCount > 0 &&
    current !== base &&
    current.trim() !== "" &&
    Number.isFinite(Number(current))
  );
}

export function RssSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();

  const settings = useQuery({
    queryKey: SETTINGS_KEY,
    queryFn: async (): Promise<RssSettings> => {
      const { data, error } = await api.GET("/settings");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return {
        rss_enabled: data.rss_enabled,
        rss_interval_s: data.rss_interval_s,
      };
    },
    retry: false,
  });
  const feeds = useQuery({
    queryKey: FEEDS_KEY,
    queryFn: async (): Promise<FeedDTO[]> => {
      const { data, error } = await api.GET("/feeds");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data.feeds ?? [];
    },
    retry: false,
  });
  const rules = useQuery({
    queryKey: RULES_KEY,
    queryFn: async (): Promise<RuleDTO[]> => {
      const { data, error } = await api.GET("/rules");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data.rules ?? [];
    },
    retry: false,
  });

  const ready =
    settings.data !== undefined &&
    feeds.data !== undefined &&
    rules.data !== undefined;

  // Both stay null until the first render with all three responses; the seed
  // runs once — later refetches update the read-only rows, not the form.
  const [form, setForm] = useState<FormState | null>(null);
  const [baseline, setBaseline] = useState<FormState | null>(null);
  const [intervalError, setIntervalError] = useState<string | null>(null);
  const savingRef = useRef(false);

  const formRef = useRef(form);
  formRef.current = form;
  const baselineRef = useRef(baseline);
  baselineRef.current = baseline;
  const feedsRef = useRef(feeds.data);
  feedsRef.current = feeds.data;

  useEffect(() => {
    if (!ready || form !== null) return;
    const seeded = seedForm(
      settings.data as RssSettings,
      feeds.data as FeedDTO[],
    );
    setForm(seeded);
    setBaseline(seeded);
  }, [ready, form, settings.data, feeds.data]);

  const feedList = feeds.data ?? [];
  const capState = commonItemCap(feedList);
  const dirtyCount =
    form === null || baseline === null
      ? 0
      : [
          form.enabled !== baseline.enabled,
          form.intervalMin !== baseline.intervalMin,
          feedList.length > 0 && form.cap !== baseline.cap,
        ].filter(Boolean).length;

  /** One PATCH /settings carrying only the changed keys, then one
   *  PATCH /feeds/{id} per feed whose item_cap differs. A rejected settings
   *  call ends the save — the feed writes stay unsent (task step 8). Only the
   *  parts that landed become the new baseline, so a rejected field keeps the
   *  form dirty. */
  const save = useCallback(() => {
    const current = formRef.current;
    const base = baselineRef.current;
    const feedRows = feedsRef.current ?? [];
    if (current === null || base === null || savingRef.current) return;
    savingRef.current = true;
    void (async () => {
      try {
        const body: { rss_enabled?: boolean; rss_interval_s?: number } = {};
        if (current.enabled !== base.enabled)
          body.rss_enabled = current.enabled;
        if (current.intervalMin !== base.intervalMin) {
          // The 300-second floor is known (doc 11 §5); an emptied or
          // sub-minimum input would be a guaranteed 422, so fail it on the
          // field now instead of shipping a doomed PATCH.
          const seconds = minutesToSeconds(Number(current.intervalMin));
          if (!Number.isFinite(seconds) || seconds < MIN_INTERVAL_S) {
            setIntervalError(
              t("rss.intervalTooSmall", { min: MIN_INTERVAL_S / 60 }),
            );
            return;
          }
          body.rss_interval_s = seconds;
        }
        if (Object.keys(body).length > 0) {
          const { error } = await api.PATCH("/settings", { body });
          if (error !== undefined) {
            // A field-scoped 422 lands on its own input; anything else is a
            // toast. Either way the feed fan-out below is skipped.
            const field = (error.errors ?? []).find((entry) =>
              entry.location?.endsWith("rss_interval_s"),
            );
            if (field?.message !== undefined) setIntervalError(field.message);
            else
              toast.error(
                t("rss.saveFailed", {
                  detail: problemDetail(error) ?? ct("shell.networkError"),
                }),
              );
            return;
          }
          await queryClient.invalidateQueries({ queryKey: SETTINGS_KEY });
        }

        const cap = Number(current.cap);
        const applies = capApplies(current.cap, base.cap, feedRows.length);
        let capLanded = true;
        if (applies) {
          const failedFeeds: string[] = [];
          let lastDetail = "";
          for (const feed of feedRows) {
            if (feed.item_cap === cap) continue;
            const { error } = await api.PATCH("/feeds/{id}", {
              params: { path: { id: feed.id } },
              body: { item_cap: cap },
            });
            if (error !== undefined) {
              failedFeeds.push(feed.title ?? feed.url);
              lastDetail = problemDetail(error) ?? ct("shell.networkError");
            }
          }
          // One toast for the whole fan-out — N identical toasts obscure the
          // single root cause.
          if (failedFeeds.length > 0) {
            capLanded = false;
            toast.error(
              t("rss.capSaveFailed", {
                feeds:
                  failedFeeds.length > 5
                    ? `${failedFeeds.slice(0, 5).join(", ")}, +${failedFeeds.length - 5} more`
                    : failedFeeds.join(", "),
                detail: lastDetail,
              }),
            );
          }
          await queryClient.invalidateQueries({ queryKey: FEEDS_KEY });
        }

        setBaseline({
          enabled: current.enabled,
          intervalMin: current.intervalMin,
          cap: applies && capLanded ? current.cap : base.cap,
        });
        // A cleared input is not a value to apply; once the rest of the save
        // has landed, show the stored cap again instead of a blank field. A
        // failed fan-out keeps the typed value so Save retries it.
        if (!applies && current.cap !== base.cap)
          setForm((prev) =>
            prev === null ? prev : { ...prev, cap: base.cap },
          );
      } finally {
        savingRef.current = false;
      }
    })();
  }, [queryClient, t, ct]);

  const revert = useCallback(() => {
    setForm(baselineRef.current);
    setIntervalError(null);
  }, []);

  useEffect(() => {
    useSettingsDirty.setState((prev) => {
      if (dirtyCount === 0)
        return prev.report === null ? prev : { report: null };
      if (prev.report?.count === dirtyCount && prev.report.section === "rss")
        return prev;
      return {
        report: { section: "rss", count: dirtyCount, save, revert },
      };
    });
  }, [dirtyCount, save, revert]);
  useEffect(() => () => useSettingsDirty.setState({ report: null }), []);

  const failed = [settings, feeds, rules].some(
    (query) => query.isError && query.data === undefined,
  );
  if (failed)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("rss.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            void settings.refetch();
            void feeds.refetch();
            void rules.refetch();
          }}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );
  if (form === null) return <></>;

  const enabledRules = (rules.data ?? []).filter((rule) => rule.enabled).length;
  const totalRules = (rules.data ?? []).length;

  return (
    <div className="flex max-w-xl flex-col gap-5">
      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="rss-enabled">{t("rss.enable")}</Label>
        <Checkbox
          id="rss-enabled"
          checked={form.enabled}
          onCheckedChange={(checked) =>
            setForm({ ...form, enabled: checked === true })
          }
        />
      </div>
      <p className="-mt-3 text-xs text-muted-foreground">
        {t("rss.enableHint")}
      </p>

      <div className="flex items-start justify-between gap-4">
        <Label htmlFor="rss-interval" className="pt-1.5">
          {t("rss.interval")}
        </Label>
        <div className="flex w-28 flex-col gap-1">
          <Input
            id="rss-interval"
            type="number"
            min={MIN_INTERVAL_S / 60}
            value={form.intervalMin}
            aria-invalid={intervalError !== null || undefined}
            aria-describedby={
              intervalError !== null ? "rss-interval-error" : undefined
            }
            onChange={(event) => {
              setForm({ ...form, intervalMin: event.target.value });
              setIntervalError(null);
            }}
          />
          <p className="text-xs text-muted-foreground">
            {t("rss.intervalHint")}
          </p>
          {intervalError !== null && (
            <p
              id="rss-interval-error"
              role="alert"
              className="text-xs text-destructive"
            >
              {intervalError}
            </p>
          )}
        </div>
      </div>

      <div className="flex items-center justify-between gap-4">
        <Label htmlFor="rss-item-cap">{t("rss.itemCap")}</Label>
        <Input
          id="rss-item-cap"
          type="number"
          min={0}
          className="w-28"
          disabled={capState === null}
          value={form.cap}
          placeholder={capState === "mixed" ? t("rss.capMixed") : undefined}
          onChange={(event) => setForm({ ...form, cap: event.target.value })}
        />
      </div>
      <p className="-mt-3 text-xs text-muted-foreground">
        {t("rss.itemCapHint")}
      </p>

      <div role="group" aria-label={t("rss.autoDownloader")}>
        <div className="flex items-center justify-between gap-4">
          <p className="text-sm font-medium">{t("rss.autoDownloader")}</p>
          <p className="text-sm text-muted-foreground">
            {t("rss.rulesEnabled", {
              enabled: enabledRules,
              total: totalRules,
            })}{" "}
            <Link to="/rss/rules" className="underline underline-offset-2">
              {t("rss.rulesLink")}
            </Link>
          </p>
        </div>
        <p className="mt-1 text-xs text-muted-foreground">
          {t("rss.rulesNote")}
        </p>
      </div>

      <div role="group" aria-label={t("rss.smartFilter")}>
        <p className="text-sm font-medium">{t("rss.smartFilter")}</p>
        <pre className="mt-1 overflow-x-auto rounded-md border border-border bg-muted/50 p-2 text-xs">
          {SMART_PATTERNS}
        </pre>
        <p className="mt-1 text-xs" style={{ color: "var(--warn)" }}>
          {t("rss.smartWarning")}
        </p>
        <p className="mt-1 text-xs text-muted-foreground">
          <Link to="/rss/rules" className="underline underline-offset-2">
            {t("rss.ruleEditorLink")}
          </Link>{" "}
          {t("rss.smartNoteTail")}
        </p>
      </div>
    </div>
  );
}
