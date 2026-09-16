import { useState, type JSX } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatAbsolute, formatWhen } from "../../lib/format";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { Input } from "../ui/input";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type TestResult = components["schemas"]["TestEngineOutputBody"];

// The plan's canonical repository documentation; not a runtime setting.
const ADR_0017_URL =
  "https://github.com/L-K-M/dl-tool/blob/main/docs/decisions/0017-exclusive-control-of-engines.md";

/** Read-only engine rows plus one Test button each. Endpoint values supplied by the environment are
 *  rendered disabled with the reason, never as an editable field. */
export function ConnectionSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const engines = useQuery({
    queryKey: ["engines"],
    queryFn: async () => {
      const { data, error } = await api.GET("/engines");
      if (error !== undefined)
        throw new Error(error.detail ?? error.title ?? error.type);
      return data?.engines ?? [];
    },
    retry: false,
  });
  const [results, setResults] = useState<Record<string, TestResult>>({});
  const [pending, setPending] = useState<Record<string, boolean>>({});

  const testEngine = async (id: string) => {
    setPending((current) => ({ ...current, [id]: true }));
    try {
      const { data, error } = await api.POST("/engines/{id}/test", {
        params: { path: { id } },
      });
      // A failed probe is a 200 with ok:false — a rendered result, never a
      // toast. Only a non-2xx problem or a transport failure raises one.
      if (data !== undefined)
        setResults((current) => ({ ...current, [id]: data }));
      else
        toast.error(
          t("connection.testRequestFailed", {
            detail: error?.detail ?? error?.title ?? error?.type,
          }),
        );
    } catch {
      toast.error(
        t("connection.testRequestFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setPending((current) => ({ ...current, [id]: false }));
    }
  };

  if (engines.isError)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("connection.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => void engines.refetch()}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );

  return (
    <div className="flex flex-col gap-4">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-border text-start text-muted-foreground">
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.name")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.kind")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.endpoint")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.status")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.version")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.capabilities")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.lastSeen")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("connection.test")}
            </th>
          </tr>
        </thead>
        <tbody>
          {engines.isLoading ? (
            <tr>
              <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                {t("connection.loading")}
              </td>
            </tr>
          ) : (engines.data ?? []).length === 0 ? (
            <tr>
              <td colSpan={8} className="px-2 py-2 text-muted-foreground">
                {t("connection.empty")}
              </td>
            </tr>
          ) : (
            (engines.data ?? []).map((engine) => {
              const result = results[engine.id];
              return (
                <tr
                  key={engine.id}
                  className="border-b border-border align-top"
                >
                  <td className="px-2 py-2">
                    <span className="font-medium">{engine.name}</span>
                    {engine.last_error !== null && (
                      <p
                        className="mt-1 text-xs"
                        style={{ color: "var(--warn)" }}
                      >
                        {engine.last_error}
                      </p>
                    )}
                    {result !== undefined && (
                      <dl className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                        <div className="flex gap-1">
                          <dt>{t("connection.resultOk")}</dt>
                          <dd>
                            {result.ok
                              ? t("connection.resultPassed")
                              : t("connection.resultFailed")}
                          </dd>
                        </div>
                        <div className="flex gap-1">
                          <dt>{t("connection.resultVersion")}</dt>
                          <dd>{result.version ?? "—"}</dd>
                        </div>
                        <div className="flex gap-1">
                          <dt>{t("connection.resultElapsed")}</dt>
                          <dd>{result.elapsed_ms}</dd>
                        </div>
                        {result.error !== null && (
                          <div className="flex gap-1">
                            <dt>{t("connection.resultError")}</dt>
                            <dd>{result.error}</dd>
                          </div>
                        )}
                      </dl>
                    )}
                  </td>
                  <td className="px-2 py-2">{engine.kind}</td>
                  <td className="px-2 py-2">
                    {engine.url === null ? (
                      "—"
                    ) : (
                      <Input
                        value={engine.url}
                        disabled
                        readOnly
                        aria-label={t("connection.endpointFor", {
                          name: engine.name,
                        })}
                        title={t("connection.envEndpoint")}
                        className="h-7 w-56 text-xs"
                      />
                    )}
                  </td>
                  <td className="px-2 py-2">
                    <span
                      className="inline-flex items-center gap-1.5"
                      title={
                        engine.connected
                          ? t("connection.connected")
                          : t("connection.disconnected")
                      }
                    >
                      <span
                        aria-hidden="true"
                        className="inline-block size-2 rounded-full"
                        style={{
                          background: engine.connected
                            ? "var(--ok)"
                            : "var(--error)",
                        }}
                      />
                      {engine.connected
                        ? t("connection.connected")
                        : t("connection.disconnected")}
                    </span>
                  </td>
                  <td className="px-2 py-2">{engine.version ?? "—"}</td>
                  <td className="px-2 py-2">
                    {(engine.capabilities ?? []).join(", ") || "—"}
                  </td>
                  <td className="px-2 py-2">
                    {engine.last_seen_at === null ? (
                      "—"
                    ) : (
                      <span title={formatAbsolute(engine.last_seen_at)}>
                        {formatWhen(engine.last_seen_at)}
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-2">
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={pending[engine.id] === true}
                      onClick={() => void testEngine(engine.id)}
                    >
                      {pending[engine.id] === true
                        ? t("connection.testing")
                        : t("connection.test")}
                    </Button>
                  </td>
                </tr>
              );
            })
          )}
        </tbody>
      </table>
      <p className="text-xs text-muted-foreground">
        {t("connection.envEndpoint")}
      </p>
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
