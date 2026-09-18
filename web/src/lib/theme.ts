import { useUiPrefs } from "../store/useUiPrefs";

export type ThemeChoice = "system" | "light" | "dark";

const DARK_QUERY = "(prefers-color-scheme: dark)";

export function readStoredTheme(): ThemeChoice {
  // The store renders defaultPrefs until hydrate lands (doc 09 §3.3), so the
  // pre-paint call in main.tsx reads "system" until the GET resolves. The
  // store is touched lazily inside the function body: useUiPrefs never
  // imports this file, so the one-way import initializes cleanly under any
  // module order.
  const theme = useUiPrefs.getState().theme;
  // The document is open, so a value outside the enum degrades to "system"
  // rather than propagating into resolveTheme.
  return theme === "light" || theme === "dark" ? theme : "system";
}

export function storeTheme(choice: ThemeChoice): void {
  // The server document is the sole store; the debounced writer carries the
  // member with the next PUT (doc 09 §3.3).
  useUiPrefs.getState().patch({ theme: choice });
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
