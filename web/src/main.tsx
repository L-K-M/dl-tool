import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
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

// Meet the install criterion and cache static assets; nothing works offline.
if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    void navigator.serviceWorker.register(new URL("sw.js", document.baseURI), {
      scope: new URL("./", document.baseURI).pathname,
    });
  });
}
