import { useTranslation } from "react-i18next";
import { formatBytes, formatEta, formatRate } from "../../lib/format";
import { useTasks } from "../../store/useTasks";
import { TaskProgress, TaskStatus } from "./TaskGrid";

function Card({ id }: { id: string }) {
  const task = useTasks((state) => state.tasks.get(id));
  const selected = useTasks((state) => state.selection.has(id));
  const { t, i18n } = useTranslation("grid");
  if (!task) return null;
  const locale = i18n.language;
  const metadata = [
    <TaskStatus task={task} />,
    formatBytes(task.total_bytes, locale),
    `↓${formatRate(task.download_rate, locale)}`,
    `↑${formatRate(task.upload_rate, locale)}`,
    task.eta_seconds === 0 ? "—" : formatEta(task.eta_seconds, locale),
  ];
  return (
    <div
      role="gridcell"
      aria-colindex={1}
      tabIndex={-1}
      style={{
        display: "flex",
        width: "100%",
        minWidth: "min-content",
        height: "100%",
        padding: "8px 0",
        boxSizing: "border-box",
        gap: 8,
        fontSize: 14,
        lineHeight: "16px",
      }}
    >
      <div
        data-testid="card-content"
        style={{
          flex: 1,
          minWidth: "min-content",
          display: "grid",
          gridTemplateRows: "32px 16px 80px",
          gap: 8,
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
            columnGap: 4,
            height: 80,
          }}
        >
          {metadata.map((value, index) => (
            <span
              key={index}
              style={{
                whiteSpace: "nowrap",
                lineHeight: "16px",
                flexShrink: 0,
              }}
            >
              {index > 0 ? "· " : ""}
              {value}
            </span>
          ))}
        </div>
      </div>
      <label
        style={{
          width: 44,
          minWidth: 44,
          height: 44,
          display: "grid",
          placeItems: "center",
        }}
      >
        <input
          type="checkbox"
          tabIndex={-1}
          readOnly
          checked={selected}
          aria-label={t("selectTask", { name: task.name })}
          style={{ width: 44, height: 44, margin: 0 }}
        />
      </label>
    </div>
  );
}

/** The grid owns scrolling and virtual offsets; cards only render its visible IDs. */
export function TaskCardList({ ids }: { ids: string[]; total: number }) {
  return (
    <>
      {ids.map((id) => (
        <Card key={id} id={id} />
      ))}
    </>
  );
}
