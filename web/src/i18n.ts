import i18next from "i18next";
import { initReactI18next } from "react-i18next";
import common from "./locales/en/common.json";

export const NAMESPACES = [
  "common",
  "grid",
  "dialogs",
  "settings",
  "rss",
  "search",
  "errors",
] as const;
export const DEFAULT_LOCALE = "en";

export function initI18n(): typeof i18next {
  if (i18next.isInitialized) return i18next;

  // Bundled resources initialize synchronously, without a network backend.
  void i18next.use(initReactI18next).init({
    lng: DEFAULT_LOCALE,
    fallbackLng: DEFAULT_LOCALE,
    supportedLngs: [DEFAULT_LOCALE],
    ns: [...NAMESPACES],
    defaultNS: "common",
    resources: { en: { common } },
    initAsync: false,
    interpolation: { escapeValue: false },
  });
  return i18next;
}
