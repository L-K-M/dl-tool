import i18next from "i18next";
import { initReactI18next } from "react-i18next";
import common from "./locales/en/common.json";
import dialogs from "./locales/en/dialogs.json";
import errors from "./locales/en/errors.json";
import grid from "./locales/en/grid.json";

export const NAMESPACES = [
  "common",
  "grid",
  "dialogs",
  "settings",
  "rss",
  "search",
  "errors",
] as const;
export type Namespace = (typeof NAMESPACES)[number];

/** en only in v1. Adding a language must be a data change, never a code change. */
export const SUPPORTED_LOCALES = ["en"] as const;
export const DEFAULT_LOCALE = SUPPORTED_LOCALES[0];

let throwOnMissing = false;

export function initI18n(opts?: { throwOnMissing?: boolean }): typeof i18next {
  throwOnMissing = opts?.throwOnMissing ?? false;
  if (i18next.isInitialized) return i18next;

  // Bundled resources initialize synchronously, without a network backend.
  void i18next.use(initReactI18next).init({
    lng: DEFAULT_LOCALE,
    fallbackLng: DEFAULT_LOCALE,
    supportedLngs: [...SUPPORTED_LOCALES],
    ns: [...NAMESPACES],
    defaultNS: "common",
    resources: { en: { common, grid, dialogs, errors } },
    initAsync: false,
    interpolation: { escapeValue: false },
    // Plurals resolve through i18next suffix keys (key_one / key_other) per
    // doc 09 §10.2; never inline ICU syntax.
    parseMissingKeyHandler: (key: string) => {
      if (throwOnMissing) throw new Error(`missing i18n key: ${key}`);
      return key;
    },
  });
  return i18next;
}

/** The active language, used as the default locale argument of every formatter. */
export function activeLocale(): string {
  return i18next.language ?? DEFAULT_LOCALE;
}
