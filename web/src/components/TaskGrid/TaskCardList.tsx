import { useTranslation } from "react-i18next";
import { formatBytes, formatEta, formatRate } from "../../lib/format";
import { useTasks, type Task } from "../../store/useTasks";
import { GridSurface, TaskProgress, TaskStatus } from "./TaskGrid";

/** Standalone card entry; TaskGrid keeps the same viewport across breakpoints. */
export function TaskCardList(props: { ids: string[]; total: number }) {
  return <GridSurface {...props} layout="cards" />;
}

export function CardContents({
  task,
  selected,
}: {
  task: Task;
  selected: boolean;
}) {
  const { t, i18n } = useTranslation("grid");
  const locale = i18n.language;
  return (
    <div
      data-testid="task-card"
      style={{
        display: "grid",
        gridTemplateColumns: "minmax(min-content, 1fr) 44px",
        gap: 8,
        padding: "8px",
        boxSizing: "border-box",
        height: 160,
        fontSize: 14,
        lineHeight: "16px",
      }}
    >
      <div
        style={{
          display: "grid",
          gridTemplateRows: "32px 16px 80px",
          gap: 8,
          minWidth: "min-content",
        }}
      >
        <span
          title={task.name}
          style={{
            display: "-webkit-box",
            WebkitLineClamp: 2,
            WebkitBoxOrient: "vertical",
            overflow: "hidden",
            overflowWrap: "anywhere",
            minWidth: 0,
          }}
        >
          {task.name}
        </span>
        <TaskProgress task={task} />
        <div
          data-testid="card-metadata"
          style={{
            display: "flex",
            flexWrap: "wrap",
            alignContent: "start",
            columnGap: 8,
            height: 80,
          }}
        >
          {[
            <TaskStatus key="status" task={task} />,
            formatBytes(task.total_bytes, locale),
            `↓${formatRate(task.download_rate, locale)}`,
            `↑${formatRate(task.upload_rate, locale)}`,
            task.eta_seconds === 0 ? "—" : formatEta(task.eta_seconds, locale),
          ].map((item, index) => (
            <span key={index} style={{ whiteSpace: "nowrap", height: 16 }}>
              {index > 0 ? "· " : ""}
              {item}
            </span>
          ))}
        </div>
      </div>
      <button
        type="button"
        tabIndex={-1}
        aria-label={t("selectTask", { name: task.name })}
        aria-pressed={selected}
        style={{ width: 44, height: 44 }}
        onClick={(event) => {
          event.stopPropagation();
          const next = new Set(useTasks.getState().selection);
          if (selected) next.delete(task.id);
          else next.add(task.id);
          useTasks.getState().setSelection(next);
        }}
      >
        <span aria-hidden="true">{selected ? "☑" : "☐"}</span>
      </button>
    </div>
  );
}
