import type { JSX } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { AlertTriangle } from "lucide-react";

import { POLL_INTERVAL_MS, useTransportUi } from "../../api/events";
import { useTasks } from "../../store/useTasks";
import { Button } from "../ui/button";

const bannerClass =
  "absolute inset-x-0 top-full z-20 flex items-center gap-2 border-b border-border px-3 py-1.5 text-sm";

/**
 * The two banners of doc 09 section 10.8: the session-expired banner for a
 * 401, and the lost-connection banner with the retry countdown. Rendered as an
 * overlay beneath the toolbar so it never disturbs the grid's row geometry.
 */
export function ReconnectBanner({
  retryNow,
}: {
  retryNow: () => void;
}): JSX.Element | null {
  const { t } = useTranslation();
  const connection = useTasks((s) => s.connection);
  const unauthenticated = useTransportUi((s) => s.unauthenticated);
  const banner = useTransportUi((s) => s.banner);
  const nextRetryIn = useTransportUi((s) => s.nextRetryIn);

  if (unauthenticated) {
    return (
      <div
        role="alert"
        className={`${bannerClass} bg-[var(--error)] text-white`}
      >
        <AlertTriangle className="size-4 shrink-0" aria-hidden="true" />
        <span>{t("shell.banner.sessionExpired")}</span>
        <Link to="/login" className="font-medium underline">
          {t("shell.banner.signIn")}
        </Link>
      </div>
    );
  }

  if (!banner) return null;

  return (
    <div role="alert" className={`${bannerClass} bg-[var(--warn)] text-white`}>
      <AlertTriangle className="size-4 shrink-0" aria-hidden="true" />
      <span>{t("shell.banner.lost")}</span>
      {nextRetryIn !== null ? (
        // The countdown mutates every second inside an alert region; it must
        // not be re-announced assertively on each tick.
        <span aria-hidden="true">
          {t("shell.banner.retryIn", { seconds: nextRetryIn })}
        </span>
      ) : null}
      {connection === "polling" ? (
        <span>
          {t("shell.banner.polling", { seconds: POLL_INTERVAL_MS / 1000 })}
        </span>
      ) : null}
      <Button
        variant="outline"
        size="sm"
        className="ml-auto text-foreground"
        onClick={retryNow}
      >
        {t("shell.banner.retryNow")}
      </Button>
    </div>
  );
}
