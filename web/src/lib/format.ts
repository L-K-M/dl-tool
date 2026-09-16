import { activeLocale } from "@/i18n";

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

// Intl formatter construction costs far more than format(), and every grid
// cell calls these per render; cache one instance per locale/options pair.
const numberFormats = new Map<string, Intl.NumberFormat>();
const dateTimeFormats = new Map<string, Intl.DateTimeFormat>();
const relativeTimeFormats = new Map<string, Intl.RelativeTimeFormat>();

function numberFormat(
  locale: string | undefined,
  options: Intl.NumberFormatOptions,
): Intl.NumberFormat {
  const key = `${locale}\0${JSON.stringify(options)}`;
  let format = numberFormats.get(key);
  if (format === undefined) {
    format = new Intl.NumberFormat(locale, options);
    numberFormats.set(key, format);
  }
  return format;
}

function dateTimeFormat(
  locale: string | undefined,
  options: Intl.DateTimeFormatOptions,
): Intl.DateTimeFormat {
  const key = `${locale}\0${JSON.stringify(options)}`;
  let format = dateTimeFormats.get(key);
  if (format === undefined) {
    format = new Intl.DateTimeFormat(locale, options);
    dateTimeFormats.set(key, format);
  }
  return format;
}

function relativeTimeFormat(
  locale: string | undefined,
): Intl.RelativeTimeFormat {
  const key = locale ?? "";
  let format = relativeTimeFormats.get(key);
  if (format === undefined) {
    format = new Intl.RelativeTimeFormat(locale);
    relativeTimeFormats.set(key, format);
  }
  return format;
}

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
  return numberFormat(locale, {
    style: "unit",
    unit,
    unitDisplay: "short",
    maximumFractionDigits: 1,
  }).format(value);
}

export function formatBytes(
  bytes: number | null,
  locale: string = activeLocale(),
): string {
  if (bytes === null) return missing;
  return formatMagnitude(bytes, "bytes", locale);
}

export function formatRate(
  bytesPerSecond: number,
  locale: string = activeLocale(),
): string {
  return formatMagnitude(bytesPerSecond, "rate", locale);
}

export function formatEta(
  seconds: number | null,
  locale: string = activeLocale(),
): string {
  if (seconds === null) return infinite;

  // Split elapsed units, leaving number rendering and labels to Intl.
  let remaining = Math.floor(seconds);
  const parts: string[] = [];
  for (const [unit, divisor] of durationUnits) {
    const value = Math.floor(remaining / divisor);
    remaining %= divisor;
    if (value === 0 && (unit !== "second" || parts.length > 0)) continue;

    parts.push(
      numberFormat(locale, {
        style: "unit",
        unit,
        unitDisplay: "narrow",
        maximumFractionDigits: 0,
      }).format(value),
    );
  }
  return parts.join(" ");
}

export function formatInteger(
  value: number,
  locale: string = activeLocale(),
): string {
  return numberFormat(locale, {}).format(value);
}

export function formatRatio(
  ratio: number,
  locale: string = activeLocale(),
): string {
  if (ratio > maxRatio) return infinite;
  return numberFormat(locale, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(ratio);
}

export function formatPercent(
  progress: number,
  locale: string = activeLocale(),
): string {
  return numberFormat(locale, {
    style: "percent",
    maximumFractionDigits: 1,
  }).format(progress);
}

export function formatWhen(
  rfc3339: string,
  now = new Date(),
  locale: string = activeLocale(),
): string {
  const date = new Date(rfc3339);
  const seconds = (date.getTime() - now.getTime()) / millisecondsPerSecond;
  if (Math.abs(seconds) >= relativeDays * secondsPerDay) {
    return dateTimeFormat(locale, { dateStyle: "medium" }).format(date);
  }

  const [unit, divisor] = durationUnits.find(
    ([, size]) => Math.abs(seconds) >= size,
  ) ?? ["second", 1];
  return relativeTimeFormat(locale).format(Math.round(seconds / divisor), unit);
}

export function formatAbsolute(
  rfc3339: string,
  locale: string = activeLocale(),
): string {
  return dateTimeFormat(locale, {
    dateStyle: "full",
    timeStyle: "long",
  }).format(new Date(rfc3339));
}
