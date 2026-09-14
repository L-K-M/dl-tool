const missing = "—";
const infinite = "∞";
const byteDivisor = 1024;
const byteUnits = [
  "byte",
  "kilobyte",
  "megabyte",
  "gigabyte",
  "terabyte",
  "petabyte",
];
const secondsPerMinute = 60;
const secondsPerHour = 60 * secondsPerMinute;
const secondsPerDay = 24 * secondsPerHour;
const durationUnits = [
  ["day", secondsPerDay],
  ["hour", secondsPerHour],
  ["minute", secondsPerMinute],
  ["second", 1],
] as const;
const relativeDays = 7;
const millisecondsPerSecond = 1000;
const maxRatio = 9999;

function formatMagnitude(
  value: number,
  kind: "bytes" | "rate",
  locale?: string,
): string {
  if (value === 0) return missing;

  let magnitude = 0;
  while (Math.abs(value) >= byteDivisor && magnitude < byteUnits.length - 1) {
    value /= byteDivisor;
    magnitude++;
  }

  const unit =
    kind === "rate"
      ? `${byteUnits[magnitude]}-per-second`
      : byteUnits[magnitude];
  return new Intl.NumberFormat(locale, {
    style: "unit",
    unit,
    unitDisplay: "short",
    maximumFractionDigits: 1,
  }).format(value);
}

export function formatBytes(bytes: number | null, locale?: string): string {
  if (bytes === null) return missing;
  return formatMagnitude(bytes, "bytes", locale);
}

export function formatRate(bytesPerSecond: number, locale?: string): string {
  return formatMagnitude(bytesPerSecond, "rate", locale);
}

export function formatEta(seconds: number | null, locale?: string): string {
  if (seconds === null) return infinite;

  // Split elapsed units, leaving number rendering and labels to Intl.
  let remaining = Math.floor(seconds);
  const parts: string[] = [];
  for (const [unit, divisor] of durationUnits) {
    const value = Math.floor(remaining / divisor);
    remaining %= divisor;
    if (value === 0 && (unit !== "second" || parts.length > 0)) continue;

    parts.push(
      new Intl.NumberFormat(locale, {
        style: "unit",
        unit,
        unitDisplay: "narrow",
        maximumFractionDigits: 0,
      }).format(value),
    );
  }
  return parts.join(" ");
}

export function formatRatio(ratio: number, locale?: string): string {
  if (ratio > maxRatio) return infinite;
  return new Intl.NumberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(ratio);
}

export function formatPercent(progress: number, locale?: string): string {
  return new Intl.NumberFormat(locale, {
    style: "percent",
    maximumFractionDigits: 1,
  }).format(progress);
}

export function formatWhen(
  rfc3339: string,
  now = new Date(),
  locale?: string,
): string {
  const date = new Date(rfc3339);
  const seconds = (date.getTime() - now.getTime()) / millisecondsPerSecond;
  if (Math.abs(seconds) >= relativeDays * secondsPerDay) {
    return new Intl.DateTimeFormat(locale, { dateStyle: "medium" }).format(
      date,
    );
  }

  const [unit, divisor] = durationUnits.find(
    ([, size]) => Math.abs(seconds) >= size,
  ) ?? ["second", 1];
  return new Intl.RelativeTimeFormat(locale).format(
    Math.round(seconds / divisor),
    unit,
  );
}

export function formatAbsolute(rfc3339: string, locale?: string): string {
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "full",
    timeStyle: "long",
  }).format(new Date(rfc3339));
}
