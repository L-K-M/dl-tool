import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import { applyTheme, readStoredTheme } from "./lib/theme";

applyTheme(readStoredTheme());

export function App() {
  return <div data-testid="app-root">dl-tool</div>;
}

const host = document.getElementById("root");
if (host)
  createRoot(host).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
