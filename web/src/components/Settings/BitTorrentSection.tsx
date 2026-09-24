import { type JSX } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";

initI18n().addResourceBundle("en", "settings", settingsStrings);

// The plan's canonical repository documentation; not a runtime setting.
const ADR_0017_URL =
  "https://github.com/L-K-M/dl-tool/blob/main/docs/decisions/0017-exclusive-control-of-engines.md";

/** Read-only by construction. docs/11-config-reference.md §5 defines no settings key for any
 *  BitTorrent protocol control, and PATCH /settings answers 422 for an unknown key, so this
 *  section names where each control lives instead of rendering a form whose Save cannot work. */
export interface BitTorrentRow {
  /** i18next key under settings.bittorrent.rows */
  labelKey: string;
  /** 'engine' = a qBittorrent daemon preference; 'task' = per task, set in the add-task dialog
   *  and PATCH /tasks/{id}. */
  livesIn: "engine" | "task";
}

export const BITTORRENT_ROWS: readonly BitTorrentRow[] = [
  { labelKey: "dht", livesIn: "engine" },
  { labelKey: "pex", livesIn: "engine" },
  { labelKey: "lsd", livesIn: "engine" },
  { labelKey: "encryption", livesIn: "engine" },
  { labelKey: "maxPeers", livesIn: "engine" },
  { labelKey: "appendTrackers", livesIn: "engine" },
  { labelKey: "shareRatioLimit", livesIn: "task" },
  { labelKey: "seedingTimeLimit", livesIn: "task" },
  { labelKey: "limitReachedAction", livesIn: "task" },
];

const ENGINES_KEY = ["engines"] as const;

function problemDetail(
  error: { detail?: string; title?: string; type?: string } | undefined,
): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

export function BitTorrentSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const engines = useQuery({
    queryKey: ENGINES_KEY,
    queryFn: async () => {
      const { data, error } = await api.GET("/engines");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data.engines ?? [];
    },
    retry: false,
  });

  // A failed background refetch keeps the previous data; only an error with
  // nothing cached replaces the whole section.
  if (engines.isError && engines.data === undefined)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("bittorrent.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => void engines.refetch()}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );

  const qbt = (engines.data ?? []).find(
    (engine) => engine.kind === "qbittorrent",
  );

  return (
    <div className="flex flex-col gap-4">
      <table className="w-full max-w-2xl text-sm">
        <thead>
          <tr className="border-b border-border text-start text-muted-foreground">
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("bittorrent.colControl")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("bittorrent.colLivesIn")}
            </th>
          </tr>
        </thead>
        <tbody>
          {BITTORRENT_ROWS.map((row) => (
            <tr key={row.labelKey} className="border-b border-border align-top">
              <td className="px-2 py-2 font-medium">
                {t(`bittorrent.rows.${row.labelKey}`)}
              </td>
              <td className="px-2 py-2 text-muted-foreground">
                {row.livesIn === "engine"
                  ? t("bittorrent.livesEngine")
                  : t("bittorrent.livesTask")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <div
        role="group"
        aria-label={t("bittorrent.engineHeading")}
        className="flex flex-col gap-1"
      >
        <h2 className="text-sm font-medium">{t("bittorrent.engineHeading")}</h2>
        {engines.isLoading ? null : qbt === undefined ? (
          <p className="text-sm text-muted-foreground">
            {t("bittorrent.engineMissing")}
          </p>
        ) : (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
            <span className="inline-flex items-center gap-1.5">
              <span
                aria-hidden="true"
                className="inline-block size-2 rounded-full"
                style={{
                  background: qbt.connected ? "var(--ok)" : "var(--error)",
                }}
              />
              {qbt.connected
                ? t("connection.connected")
                : t("connection.disconnected")}
            </span>
            <span className="text-muted-foreground">
              {t("connection.version")}: {qbt.version ?? "—"}
            </span>
          </div>
        )}
        {qbt !== undefined && qbt.last_error !== null && (
          <p className="text-sm" style={{ color: "var(--warn)" }}>
            {qbt.last_error}
          </p>
        )}
      </div>

      <p className="text-xs text-muted-foreground">
        {t("connection.exclusiveNote")}{" "}
        <a
          href={ADR_0017_URL}
          target="_blank"
          rel="noreferrer"
          className="underline underline-offset-2"
        >
          {t("connection.adrLink")}
        </a>
      </p>
    </div>
  );
}
