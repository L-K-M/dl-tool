import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

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
