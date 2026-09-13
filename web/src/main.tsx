import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
export { default as App } from "./App";
import "./index.css";
import { applyTheme, readStoredTheme } from "./lib/theme";

// Set the document theme before React can paint.
applyTheme(readStoredTheme());

const host = document.getElementById("root");
if (host)
  createRoot(host).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
