import { useSyncExternalStore } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Turtle } from "lucide-react";
import { formatRate } from "../../lib/format";
import { selectStats, useTasks } from "../../store/useTasks";

export type ScheduleMode = "default" | "alt" | "no-dl";

export interface ScheduleSummary {
  enabled: boolean;
  mode: ScheduleMode;
}

/**
 * GET /settings/schedule does not exist yet (T080 owns the endpoint, T118 the wiring).
 * Until it does, segment 5 renders an em dash and the turtle stays hidden; tests and
 * the future wiring set the summary here, so no other component changes then.
 */
let scheduleSummary: ScheduleSummary | null = null;
const scheduleListeners = new Set<() => void>();

export function setScheduleSummary(next: ScheduleSummary | null): void {
  scheduleSummary = next;
  for (const listener of scheduleListeners) listener();
}

function useScheduleSummary(): ScheduleSummary | null {
  return useSyncExternalStore(
    (onStoreChange) => {
      scheduleListeners.add(onStoreChange);
      return () => scheduleListeners.delete(onStoreChange);
    },
    () => scheduleSummary,
    () => scheduleSummary,
  );
}

const connectionByState = {
  live: { dot: "●", key: "connected" },
  connecting: { dot: "◐", key: "reconnecting" },
  polling: { dot: "◐", key: "reconnecting" },
  offline: { dot: "○", key: "offline" },
} as const;

export function StatusBar() {
  const { t, i18n } = useTranslation();
  const stats = useTasks(selectStats);
  const total = useTasks((state) => state.tasks.size);
  const connection = useTasks((state) => state.connection);
  const schedule = useScheduleSummary();
  const locale = i18n.language;
  const numbers = new Intl.NumberFormat(locale);
  const live = connectionByState[connection];
  const scheduleLabel = !schedule
    ? "—"
    : !schedule.enabled
      ? t("status.schedOff")
      : t(`status.sched_${schedule.mode}`);
  // GET /api/v1/fs/free-space arrives with T047; until then the segment is an em dash.
  const freeSpace = "—";

  return (
    <div
      className="flex h-full items-center gap-4 px-2 text-xs"
      style={{ fontVariantNumeric: "tabular-nums" }}
    >
      <span role="status" aria-live="polite" data-segment="connection">
        <span aria-hidden="true">{live.dot}</span> {t(`status.${live.key}`)}
      </span>
      <span data-segment="rates">
        <span aria-hidden="true">↓</span> {formatRate(stats.speed_down, locale)}{" "}
        <span aria-hidden="true">↑</span> {formatRate(stats.speed_up, locale)}
      </span>
      <span data-segment="counts">
        {t("status.activeOfTotal", {
          active: numbers.format(stats.active),
          total: numbers.format(total),
        })}
      </span>
      <span data-segment="free-space">
        {t("status.freeSpace", { value: freeSpace })}
      </span>
      <Link
        to="/settings/bandwidth"
        data-segment="schedule"
        className="hover:underline"
      >
        {t("status.schedule")}: {scheduleLabel}
      </Link>
      {schedule?.enabled && schedule.mode === "alt" && (
        <Link
          to="/settings/bandwidth"
          data-segment="alt-speed"
          aria-label={t("status.altSpeed")}
          title={t("status.altSpeed")}
          className="flex items-center gap-1 hover:underline"
        >
          <Turtle size={14} aria-hidden="true" />
        </Link>
      )}
    </div>
  );
}
