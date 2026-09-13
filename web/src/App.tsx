import { createContext, useContext, useState, type ReactNode } from "react";
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { I18nextProvider, useTranslation } from "react-i18next";
import {
  BrowserRouter,
  Navigate,
  Outlet,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";
import { api, basePath, setCsrfToken } from "./api/client";
import type { components } from "./api/schema";
import { LoginScreen } from "./components/Auth/LoginScreen";
import { SetupScreen } from "./components/Auth/SetupScreen";
import { Toaster } from "./components/ui/sonner";
import { initI18n } from "./i18n";

export const AUTH_STATUS = {
  unauthorized: 401,
  conflict: 409,
  rateLimited: 429,
} as const;
const sessionKey = ["session"] as const;
type AuthEnvelope = components["schemas"]["AuthEnvelope"];
export type SessionState =
  | { status: "loading" }
  | { status: "setup-required" }
  | { status: "anonymous" }
  | { status: "authenticated"; user: AuthEnvelope["user"]; csrfToken: string };
const SessionContext = createContext<SessionState | null>(null);

function authenticated(envelope: AuthEnvelope): SessionState {
  setCsrfToken(envelope.csrf_token);
  return {
    status: "authenticated",
    user: envelope.user,
    csrfToken: envelope.csrf_token,
  };
}

async function loadSession(): Promise<SessionState> {
  setCsrfToken(null);
  const { data, error, response } = await api.GET("/auth/me");
  if (data) return authenticated(data);
  if (response.status !== AUTH_STATUS.unauthorized)
    throw new Error("Session unavailable");
  return {
    status:
      error?.type === "/problems/setup-required"
        ? "setup-required"
        : "anonymous",
  };
}

/** One cached boot request, shared by gates and forms, including StrictMode remounts. */
export function useSession(): SessionState {
  const session = useContext(SessionContext);
  if (!session) throw new Error("Session provider missing");
  return session;
}

// Mutations replace the boot result so stale anonymous state cannot redirect a new session.
export function useSessionUpdate() {
  const client = useQueryClient();
  return (envelope: AuthEnvelope | null) => {
    if (!envelope) setCsrfToken(null);
    client.setQueryData<SessionState>(
      sessionKey,
      envelope ? authenticated(envelope) : { status: "anonymous" },
    );
  };
}

function SessionProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const query = useQuery({
    queryKey: sessionKey,
    queryFn: loadSession,
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  if (query.isError) {
    return (
      <main>
        <p role="alert">{t("auth.unavailable")}</p>
        <button onClick={() => void query.refetch()}>
          {t("actions.retry")}
        </button>
      </main>
    );
  }
  return (
    <SessionContext.Provider value={query.data ?? { status: "loading" }}>
      {children}
    </SessionContext.Provider>
  );
}

export function safeNext(raw: string | null): string {
  // Backslashes and controls can become URL separators after browser normalization.
  if (
    !raw?.startsWith("/") ||
    raw.startsWith("//") ||
    /[\\\s\u0000-\u001f\u007f]/u.test(raw)
  )
    return "/";
  return raw;
}

export function RequireAuth({ children }: { children: ReactNode }) {
  const session = useSession();
  const location = useLocation();
  if (session.status === "setup-required")
    return <Navigate to="/setup" replace />;
  if (session.status !== "authenticated") {
    const next = location.pathname + location.search + location.hash;
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
  }
  return <>{children}</>;
}

function AppLayout() {
  const { t } = useTranslation();
  return (
    <div className="grid h-dvh min-w-0 grid-cols-[220px_minmax(0,1fr)] grid-rows-[48px_minmax(0,1fr)_28px] overflow-hidden">
      <header aria-label={t("auth.header")} className="col-span-2" />
      <aside aria-label={t("auth.sidebar")} />
      <main className="min-w-0 overflow-auto">
        <Outlet />
      </main>
      <footer aria-label={t("auth.statusBar")} className="col-span-2" />
    </div>
  );
}

function Placeholder({ screen }: { screen: string }) {
  const { t } = useTranslation();
  return <h1>{t(`auth.screens.${screen}`)}</h1>;
}

function TasksRoute({ filter }: { filter?: "all" }) {
  return (
    <div data-filter={filter}>
      <Placeholder screen="tasks" />
    </div>
  );
}

function AuthRoutes() {
  const session = useSession();
  const { t } = useTranslation();
  if (session.status === "loading")
    return (
      <div data-testid="app-root" role="status">
        {t("appTitle")}
      </div>
    );
  return (
    <Routes>
      <Route path="/setup" element={<SetupScreen />} />
      <Route path="/login" element={<LoginScreen />} />
      <Route
        element={
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        }
      >
        <Route path="/" element={<TasksRoute filter="all" />} />
        <Route path="/tasks/:filter" element={<TasksRoute />} />
        <Route path="/tasks/category/:name" element={<TasksRoute />} />
        <Route path="/tasks/tag/:name" element={<TasksRoute />} />
        <Route path="/search" element={<Placeholder screen="search" />} />
        <Route path="/rss/feeds" element={<Placeholder screen="rss-feeds" />} />
        <Route path="/rss/rules" element={<Placeholder screen="rss-rules" />} />
        <Route
          path="/settings/:section"
          element={<Placeholder screen="settings" />}
        />
        <Route path="/logs" element={<Placeholder screen="logs" />} />
        <Route path="*" element={<Placeholder screen="notFound" />} />
      </Route>
    </Routes>
  );
}

export default function App() {
  const [client] = useState(() => new QueryClient());
  return (
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={initI18n()}>
        <BrowserRouter basename={basePath()}>
          <SessionProvider>
            <AuthRoutes />
          </SessionProvider>
          <Toaster theme="system" />
        </BrowserRouter>
      </I18nextProvider>
    </QueryClientProvider>
  );
}
