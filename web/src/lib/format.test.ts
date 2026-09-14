import { expect, test, vi, afterEach } from "vitest";
import {
  formatAbsolute,
  formatBytes,
  formatEta,
  formatPercent,
  formatRate,
  formatRatio,
  formatWhen,
} from "./format";

afterEach(() => vi.useRealTimers());

test("TestFormatBytesMatchesSpecExamples", () => {
  expect(formatBytes(442381537280, "en")).toBe("412 GB");
  expect(formatRate(12.4 * 1024 ** 2, "en")).toBe("12.4 MB/s");
  expect(formatEta(372, "en")).toBe("6m 12s");
  expect(formatPercent(0.784, "en")).toBe("78.4%");
});

test("TestNullAndZeroRenderings", () => {
  expect(formatBytes(null, "en")).toBe("—");
  expect(formatBytes(0, "en")).toBe("—");
  expect(formatRate(0, "en")).toBe("—");
  expect(formatEta(null, "en")).toBe("∞");
  expect(formatEta(0, "en")).toBe("0s");
  expect(formatRatio(10000, "en")).toBe("∞");
  expect(formatRatio(0, "en")).toBe("0.00");
  expect(formatPercent(0, "en")).toBe("0%");
});

test("TestFormatAbsoluteUsesLocale", () => {
  const input = "2026-09-01T14:02:11Z";
  for (const locale of ["en", "de"]) {
    expect(formatAbsolute(input, locale)).toBe(
      new Intl.DateTimeFormat(locale, {
        dateStyle: "full",
        timeStyle: "long",
      }).format(new Date(input)),
    );
  }
  expect(formatAbsolute(input, "en")).not.toBe(formatAbsolute(input, "de"));
});

test("TestMagnitudeBoundariesAndLocales", () => {
  const units = [
    "byte",
    "kilobyte",
    "megabyte",
    "gigabyte",
    "terabyte",
    "petabyte",
  ];
  for (const locale of ["en", "de"]) {
    for (const [magnitude, unit] of units.entries()) {
      const value = 1.5 * 1024 ** magnitude;
      const options = {
        style: "unit",
        unitDisplay: "short",
        maximumFractionDigits: 1,
      } as const;
      expect(formatBytes(value, locale)).toBe(
        new Intl.NumberFormat(locale, { ...options, unit }).format(1.5),
      );
      expect(formatRate(value, locale)).toBe(
        new Intl.NumberFormat(locale, {
          ...options,
          unit: `${unit}-per-second`,
        }).format(1.5),
      );
      expect(formatBytes(1024 ** magnitude, locale)).toBe(
        new Intl.NumberFormat(locale, { ...options, unit }).format(1),
      );
    }
  }
  expect(formatBytes(1023, "en")).toBe("1,023 byte");
});

test("TestDurationRatioAndPercentUseIntl", () => {
  for (const locale of ["en", "de"]) {
    const duration = (
      [
        ["day", 1],
        ["hour", 2],
        ["minute", 3],
        ["second", 4],
      ] as const
    )
      .map(([unit, value]) =>
        new Intl.NumberFormat(locale, {
          style: "unit",
          unit,
          unitDisplay: "narrow",
          maximumFractionDigits: 0,
        }).format(value),
      )
      .join(" ");
    expect(formatEta(93784, locale)).toBe(duration);
    expect(formatEta(60, locale)).toBe(
      new Intl.NumberFormat(locale, {
        style: "unit",
        unit: "minute",
        unitDisplay: "narrow",
      }).format(1),
    );
    for (const ratio of [1, 1.235, 9999]) {
      expect(formatRatio(ratio, locale)).toBe(
        new Intl.NumberFormat(locale, {
          minimumFractionDigits: 2,
          maximumFractionDigits: 2,
        }).format(ratio),
      );
    }
    expect(formatPercent(0.784, locale)).toBe(
      new Intl.NumberFormat(locale, {
        style: "percent",
        maximumFractionDigits: 1,
      }).format(0.784),
    );
  }
});

test("TestRelativeDateUnitsAndSevenDayBoundary", () => {
  const now = new Date("2026-09-10T12:00:00Z");
  const day = 86400;
  for (const locale of ["en", "de"]) {
    for (const [seconds, unit, value] of [
      [0, "second", 0],
      [30, "second", 30],
      [60, "minute", 1],
      [3600, "hour", 1],
      [day, "day", 1],
      [6 * day, "day", 6],
    ] as const) {
      for (const direction of [-1, 1]) {
        const date = new Date(
          now.getTime() + direction * seconds * 1000,
        ).toISOString();
        expect(formatWhen(date, now, locale)).toBe(
          new Intl.RelativeTimeFormat(locale).format(
            seconds === 0 ? 0 : direction * value,
            unit,
          ),
        );
      }
    }
    for (const seconds of [-7 * day, 7 * day, -8 * day]) {
      const date = new Date(now.getTime() + seconds * 1000);
      expect(formatWhen(date.toISOString(), now, locale)).toBe(
        new Intl.DateTimeFormat(locale, { dateStyle: "medium" }).format(date),
      );
    }
  }
  vi.useFakeTimers();
  vi.setSystemTime(now);
  expect(formatWhen(now.toISOString(), undefined, "en")).toBe("in 0 seconds");
});
