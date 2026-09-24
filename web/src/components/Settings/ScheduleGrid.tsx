import {
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type JSX,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { useTranslation } from "react-i18next";
import { initI18n } from "../../i18n";
import settingsStrings from "../../locales/en/settings.json";

initI18n().addResourceBundle("en", "settings", settingsStrings);

/** The wire encoding of doc 05 §11.2: 0 no download, 1 default speed, 2 alternative speed. */
export type Brush = 0 | 1 | 2;

/** Exactly 168 entries, index day * 24 + hour, day 0 = Monday, hour 0..23, in the schedule's
 *  timezone. Any other length is a bug the server rejects with 422. */
export type Cells = Brush[];

export const DAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"] as const;

const HOURS_IN_DAY = 24;
const CELL_COUNT = DAYS.length * HOURS_IN_DAY;
const HOURS = Array.from({ length: HOURS_IN_DAY }, (_, hour) => hour);

export function idx(day: number, hour: number): number {
  return day * HOURS_IN_DAY + hour;
}

const dayOf = (cell: number): number => Math.floor(cell / HOURS_IN_DAY);
const hourOf = (cell: number): number => cell % HOURS_IN_DAY;
const hourLabel = (hour: number): string => String(hour).padStart(2, "0");

/** The locale keys pairing each state with its legend/brush name and the
 *  sentence-case name used inside the cell aria-label (doc 09 §9.1 renders
 *  "Monday 14:00 — Default speed"). */
const STATE_NAME: Record<Brush, string> = {
  0: "bandwidth.stateNoDownload",
  1: "bandwidth.stateDefault",
  2: "bandwidth.stateAlt",
};
const CELL_STATE_NAME: Record<Brush, string> = {
  0: "bandwidth.cellStateNoDownload",
  1: "bandwidth.cellStateDefault",
  2: "bandwidth.cellStateAlt",
};

/** Colour alone never carries a state: No Download is hatched on --error,
 *  Default Speed is solid --ok, Alternative Speed is dotted on --warn. The
 *  legend reuses these same swatches. */
export const CELL_FILL: Record<Brush, CSSProperties> = {
  0: {
    backgroundColor: "var(--bg-elevated)",
    backgroundImage:
      "repeating-linear-gradient(135deg, var(--error) 0 3px, transparent 3px 9px)",
  },
  1: { backgroundColor: "var(--ok)" },
  2: {
    backgroundColor: "var(--bg-elevated)",
    backgroundImage: "radial-gradient(var(--warn) 2.5px, transparent 3px)",
    backgroundSize: "8px 8px",
  },
};

export { STATE_NAME };

/** Every helper is pure and total: it returns a new 168-entry array and mutates nothing. */
export function paintRect(
  cells: Cells,
  from: number,
  to: number,
  brush: Brush,
): Cells {
  const next = cells.slice();
  const dayLo = Math.min(dayOf(from), dayOf(to));
  const dayHi = Math.max(dayOf(from), dayOf(to));
  const hourLo = Math.min(hourOf(from), hourOf(to));
  const hourHi = Math.max(hourOf(from), hourOf(to));
  for (let day = dayLo; day <= dayHi; day++)
    for (let hour = hourLo; hour <= hourHi; hour++)
      next[idx(day, hour)] = brush;
  return next;
}

export function paintDay(cells: Cells, day: number, brush: Brush): Cells {
  return paintRect(cells, idx(day, 0), idx(day, HOURS_IN_DAY - 1), brush);
}

export function paintHour(cells: Cells, hour: number, brush: Brush): Cells {
  return paintRect(cells, idx(0, hour), idx(DAYS.length - 1, hour), brush);
}

/** The four bulk buttons of doc 09 §9.1.
 *  fillAll              → every cell becomes brush.
 *  clearAll             → every cell becomes 1 (Default Speed), the value 00001_init.sql seeds.
 *  copyMondayToWeekdays → Tue..Fri become a copy of Mon; Sat and Sun are untouched.
 *  invert               → 0 becomes 1 and 1 becomes 0; 2 is left alone, so an alternative-speed
 *                         block survives an invert. */
export function fillAll(cells: Cells, brush: Brush): Cells {
  return new Array<Brush>(CELL_COUNT).fill(brush);
}
export function clearAll(cells: Cells): Cells {
  return fillAll(cells, 1);
}
export function copyMondayToWeekdays(cells: Cells): Cells {
  const next = cells.slice();
  for (let day = 1; day <= 4; day++)
    for (let hour = 0; hour < HOURS_IN_DAY; hour++)
      next[idx(day, hour)] = cells[idx(0, hour)] ?? 1;
  return next;
}
export function invert(cells: Cells): Cells {
  return cells.map((cell): Brush => (cell === 2 ? 2 : cell === 0 ? 1 : 0));
}

/** A 24-column by 7-row table. Cells are 28 x 28 px, role="gridcell", labelled
 *  aria-label="Monday 14:00 — Default speed". Pointer: mousedown then drag paints; Shift+drag paints
 *  the rectangle from the anchor; a row header paints that day; a column header paints that hour
 *  across every day. Keyboard: exactly one cell holds tabindex="0", arrows move it, Space applies the
 *  brush, Shift+arrow extends the rectangle from the anchor and paints it. */
export function ScheduleGrid(props: {
  cells: Cells;
  brush: Brush;
  disabled: boolean;
  onChange: (next: Cells) => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  const { cells, brush, disabled, onChange } = props;
  const [focusCell, setFocusCell] = useState(0);
  const tableRef = useRef<HTMLTableElement | null>(null);
  // The gesture state lives in refs: a drag can outrun React's render cycle,
  // so each move paints on top of the array the previous move emitted rather
  // than the props, which may still be one paint behind.
  const dragging = useRef(false);
  const anchor = useRef(0);
  const painted = useRef<Cells | null>(null);

  const emit = (next: Cells) => {
    painted.current = next;
    onChange(next);
  };

  // pointerup outside the grid ends the gesture too, so it is listened for on
  // the window rather than on the table.
  useEffect(() => {
    const end = () => {
      dragging.current = false;
      painted.current = null;
    };
    window.addEventListener("pointerup", end);
    window.addEventListener("pointercancel", end);
    return () => {
      window.removeEventListener("pointerup", end);
      window.removeEventListener("pointercancel", end);
    };
  }, []);

  const moveFocus = (next: number) => {
    setFocusCell(next);
    tableRef.current
      ?.querySelector<HTMLElement>(`[data-cell="${next}"]`)
      ?.focus();
  };

  const onCellPointerDown = (
    cell: number,
    event: ReactPointerEvent<HTMLElement>,
  ) => {
    if (disabled) return;
    anchor.current = cell;
    dragging.current = true;
    painted.current = cells;
    emit(paintRect(cells, cell, cell, brush));
    setFocusCell(cell);
    event.currentTarget.focus();
  };

  const onCellPointerEnter = (
    cell: number,
    event: ReactPointerEvent<HTMLElement>,
  ) => {
    if (disabled || !dragging.current) return;
    const base = painted.current ?? cells;
    emit(
      event.shiftKey
        ? paintRect(base, anchor.current, cell, brush)
        : paintRect(base, cell, cell, brush),
    );
  };

  const onCellKeyDown = (
    cell: number,
    event: ReactKeyboardEvent<HTMLElement>,
  ) => {
    if (disabled) return;
    const day = dayOf(cell);
    const hour = hourOf(cell);
    let next: number | null = null;
    switch (event.key) {
      case "ArrowLeft":
        if (hour > 0) next = cell - 1;
        break;
      case "ArrowRight":
        if (hour < HOURS_IN_DAY - 1) next = cell + 1;
        break;
      case "ArrowUp":
        if (day > 0) next = cell - HOURS_IN_DAY;
        break;
      case "ArrowDown":
        if (day < DAYS.length - 1) next = cell + HOURS_IN_DAY;
        break;
      case " ":
        // Space paints the focused cell and drops the keyboard anchor on it.
        event.preventDefault();
        anchor.current = cell;
        emit(paintRect(cells, cell, cell, brush));
        return;
      default:
        return;
    }
    event.preventDefault();
    if (next === null) return;
    if (event.shiftKey) emit(paintRect(cells, anchor.current, next, brush));
    else anchor.current = next;
    moveFocus(next);
  };

  return (
    <table
      ref={tableRef}
      role="grid"
      aria-label={t("bandwidth.gridLabel")}
      aria-disabled={disabled || undefined}
      aria-rowcount={DAYS.length + 1}
      aria-colcount={HOURS_IN_DAY + 1}
      className={`border-collapse${disabled ? " opacity-50" : ""}`}
    >
      <thead>
        <tr role="row">
          <th role="columnheader" className="p-0">
            <span className="sr-only">{t("bandwidth.dayColumn")}</span>
          </th>
          {HOURS.map((hour) => (
            <th
              key={hour}
              role="columnheader"
              className="p-0 text-center font-normal"
            >
              <button
                type="button"
                tabIndex={-1}
                disabled={disabled}
                onClick={() => onChange(paintHour(cells, hour, brush))}
                className="size-7 text-[10px] text-muted-foreground hover:bg-muted disabled:opacity-50"
              >
                {hourLabel(hour)}
              </button>
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {DAYS.map((day, d) => (
          <tr key={day} role="row">
            <th
              scope="row"
              role="rowheader"
              className="p-0 pe-1 text-end font-normal"
            >
              <button
                type="button"
                tabIndex={-1}
                disabled={disabled}
                onClick={() => onChange(paintDay(cells, d, brush))}
                className="h-7 px-1 text-xs text-muted-foreground hover:bg-muted disabled:opacity-50"
              >
                {t(`bandwidth.days.${day}`)}
              </button>
            </th>
            {HOURS.map((hour) => {
              const cell = idx(d, hour);
              const state = cells[cell] ?? 1;
              return (
                <td
                  key={hour}
                  role="gridcell"
                  data-cell={cell}
                  tabIndex={cell === focusCell ? 0 : -1}
                  aria-label={t("bandwidth.cellLabel", {
                    day: t(`bandwidth.daysFull.${day}`),
                    hour: hourLabel(hour),
                    state: t(CELL_STATE_NAME[state]),
                  })}
                  style={{ width: 28, height: 28, ...CELL_FILL[state] }}
                  className="border border-border p-0 focus-visible:outline-2 focus-visible:outline-offset-[-2px] focus-visible:outline-[var(--focus-ring)]"
                  onPointerDown={(event) => onCellPointerDown(cell, event)}
                  onPointerEnter={(event) => onCellPointerEnter(cell, event)}
                  onKeyDown={(event) => onCellKeyDown(cell, event)}
                />
              );
            })}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
