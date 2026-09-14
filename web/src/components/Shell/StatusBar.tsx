import type { JSX } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { formatRate } from "../../lib/format";
import { useTasks } from "../../store/useTasks";

const missing = "—";

export function StatusBar(): JSX.Element {
  const { t, i18n } = useTranslation();
  const connection = useTasks((state) => state.connection);
  const stats = useTasks((state) => state.stats);
  const total = useTasks((state) => state.tasks.size);
  const locale = i18n.language;
  const connectionText =
    connection === "live"
      ? t("shell.status.connected")
      : connection === "offline"
        ? t("shell.status.offline")
        : t("shell.status.reconnecting");
  const connectionGlyph =
    connection === "live" ? "●" : connection === "offline" ? "○" : "◐";
  return (
    <div className="flex h-full items-center gap-4 border-t border-border px-3 text-xs tabular-nums">
      <span role="status" aria-live="polite">
        {/* The shape is decorative; the translated text announces state. */}
        <span aria-hidden="true">{connectionGlyph}</span> {connectionText}
      </span>
      <span>
        ↓ {formatRate(stats.speed_down, locale)} ↑{" "}
        {formatRate(stats.speed_up, locale)}
      </span>
      <span>{t("shell.status.counts", { active: stats.active, total })}</span>
      {/* GET /fs/free-space lands with T047; until it serves, the segment
          renders the em-dash placeholder of doc 09 section 2.6. */}
      <span>{t("shell.status.free", { value: missing })}</span>
      {/* The schedule's enabled flag and active mode have no API source yet;
          the segment keeps its link target and shows the same placeholder. */}
      <Link to="/settings/bandwidth" className="hover:underline">
        {t("shell.status.schedule", { value: missing })}
      </Link>
      {/* Segment 6 shows the alt-speed turtle only while schedule_enabled is
          true; nothing reports that flag yet, so the slot stays empty. */}
      <span aria-hidden="true" />
    </div>
  );
}
