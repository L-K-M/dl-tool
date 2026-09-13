export type ThemeChoice = "system" | "light" | "dark";

const PREFS_KEY = "dl.ui.prefs.v1";
const DARK_QUERY = "(prefers-color-scheme: dark)";

function readPreferences(): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(
      localStorage.getItem(PREFS_KEY) ?? "null",
    );
    if (value && typeof value === "object" && !Array.isArray(value)) {
      return value as Record<string, unknown>;
    }
  } catch {
    // Storage can be unavailable or corrupt; boot with system defaults.
  }
  return {};
}

export function readStoredTheme(): ThemeChoice {
  const { theme } = readPreferences();
  return theme === "light" || theme === "dark" ? theme : "system";
}

export function storeTheme(choice: ThemeChoice): void {
  // Preserve preferences owned by other UI features. Write failures reach the caller.
  localStorage.setItem(
    PREFS_KEY,
    JSON.stringify({ ...readPreferences(), theme: choice }),
  );
}

export function resolveTheme(choice: ThemeChoice): "light" | "dark" {
  if (choice !== "system") return choice;
  return window.matchMedia(DARK_QUERY).matches ? "dark" : "light";
}

export function applyTheme(choice: ThemeChoice): void {
  document.documentElement.classList.toggle(
    "dark",
    resolveTheme(choice) === "dark",
  );
}
