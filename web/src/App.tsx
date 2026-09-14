import {
  createContext,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
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
  useParams,
  useSearchParams,
} from "react-router-dom";
import { toast } from "sonner";
import { api, basePath, setCsrfToken } from "./api/client";
import type { components } from "./api/schema";
import { LoginScreen } from "./components/Auth/LoginScreen";
import { SetupScreen } from "./components/Auth/SetupScreen";
import { Toaster } from "./components/ui/sonner";
import { initI18n } from "./i18n";
import { readStoredTheme } from "./lib/theme";
import { TaskGrid } from "./components/TaskGrid/TaskGrid";
import {
  RemoveTasksDialog,
  ShellActionsContext,
  ShortcutsDialog,
  Toolbar,
  focusNameFilter,
  type RemoveRequest,
  type ShellActions,
} from "./components/Shell/Toolbar";
import { Sidebar } from "./components/Shell/Sidebar";
import { StatusBar } from "./components/Shell/StatusBar";
import type { SidebarFilter } from "./store/useTasks";

type AuthEnvelope = components["schemas"]["AuthEnvelope"];
export type SessionState =
  | { status: "loading" }
  | { status: "setup-required" }
  | { status: "anonymous" }
  | { status: "authenticated"; user: AuthEnvelope["user"]; csrfToken: string };
const sessionKey = ["session"] as const;
const unauthorized = 401;
const SessionContext = createContext<SessionState>({ status: "loading" });

function authenticatedSession(envelope: AuthEnvelope): SessionState {
  setCsrfToken(envelope.csrf_token);
  return {
    status: "authenticated",
    user: envelope.user,
    csrfToken: envelope.csrf_token,
  };
}

/** Share one boot request, including React StrictMode's effect replay. */
function SessionProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const query = useQuery({
    queryKey: sessionKey,
    queryFn: async (): Promise<SessionState> => {
      const { data, error, response } = await api.GET("/auth/me");
      if (data) return authenticatedSession(data);
      if (response.status !== unauthorized)
        throw new Error("Session request failed");

      setCsrfToken(null);
      return {
        status:
          error?.type === "/problems/setup-required"
            ? "setup-required"
            : "anonymous",
      };
    },
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });
  if (query.isError)
    return (
      <div role="alert">
        {t("auth.connectionError")}{" "}
        <button onClick={() => void query.refetch()}>
          {t("actions.retry")}
        </button>
      </div>
    );

  return (
    <SessionContext.Provider value={query.data ?? { status: "loading" }}>
      {children}
    </SessionContext.Provider>
  );
}

export function useSession(): SessionState {
  return useContext(SessionContext);
}

/** Update the same cache the gates read; never infer a session from cookies. */
export function useAuthActions() {
  const queryClient = useQueryClient();
  return {
    authenticate: (envelope: AuthEnvelope) =>
      queryClient.setQueryData(sessionKey, authenticatedSession(envelope)),
    setupComplete: () => {
      setCsrfToken(null);
      queryClient.setQueryData<SessionState>(sessionKey, {
        status: "anonymous",
      });
    },
    signOut: () => {
      setCsrfToken(null);
      queryClient.setQueryData<SessionState>(sessionKey, {
        status: "anonymous",
      });
    },
  };
}

export function safeNext(raw: string | null): string {
  // Backslashes and control characters can turn a local-looking URL into an authority.
  if (
    !raw?.startsWith("/") ||
    raw.startsWith("//") ||
    /[\\\u0000-\u0020\u007f]/u.test(raw)
  )
    return "/";
  return raw;
}

function Loading() {
  const { t } = useTranslation();
  return <p role="status">{t("auth.loading")}</p>;
}

export function RequireAuth({ children }: { children: ReactNode }) {
  const session = useSession();
  const location = useLocation();
  if (session.status === "loading") return <Loading />;
  if (session.status === "setup-required")
    return <Navigate to="/setup" replace />;
  if (session.status === "anonymous") {
    const next = location.pathname + location.search + location.hash;
    return <Navigate to={`/login?${new URLSearchParams({ next })}`} replace />;
  }
  return <>{children}</>;
}

function AuthRoute({ screen }: { screen: "setup" | "login" }) {
  const session = useSession();
  const [params] = useSearchParams();
  if (session.status === "loading") return <Loading />;
  // Both the gate and submit handler honor next, so a cache update cannot race the redirect.
  if (session.status === "authenticated")
    return (
      <Navigate
        to={screen === "login" ? safeNext(params.get("next")) : "/"}
        replace
      />
    );
  if (session.status === "setup-required")
    return screen === "setup" ? (
      <SetupScreen />
    ) : (
      <Navigate to="/setup" replace />
    );
  return screen === "login" ? (
    <LoginScreen />
  ) : (
    <Navigate to="/login" replace />
  );
}

function AppLayout() {
  const { t } = useTranslation();
  const session = useSession();
  const auth = useAuthActions();
  const [removeRequest, setRemoveRequest] = useState<RemoveRequest | null>(
    null,
  );
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const actions = useMemo<ShellActions>(
    () => ({
      requestRemove: (ids, deleteFiles) =>
        setRemoveRequest({ ids, deleteFiles }),
      focusFilter: focusNameFilter,
      showShortcuts: () => setShortcutsOpen(true),
      signOut: () => {
        // Local state drops only after the server confirms the session is
        // gone; a failed or unreachable logout must not look signed out.
        void (async () => {
          try {
            const { error } = await api.POST("/auth/logout");
            if (error) {
              toast.error(
                t("shell.signOutFailed", {
                  detail: error.detail ?? error.title ?? error.type,
                }),
              );
              return;
            }
            auth.signOut();
          } catch {
            toast.error(
              t("shell.signOutFailed", {
                detail: t("shell.networkError"),
              }),
            );
          }
        })();
      },
      userName: session.status === "authenticated" ? session.user.username : "",
    }),
    [auth, session, t],
  );
  return (
    <ShellActionsContext.Provider value={actions}>
      <div
        data-testid="app-root"
        className="grid h-dvh grid-cols-[220px_minmax(0,1fr)] grid-rows-[48px_minmax(0,1fr)_28px] overflow-hidden"
      >
        <header aria-label={t("regions.header")} className="col-span-2">
          <Toolbar />
        </header>
        <aside aria-label={t("regions.sidebar")}>
          <Sidebar />
        </aside>
        <main className="min-w-0 overflow-hidden">
          <Outlet />
        </main>
        <footer aria-label={t("regions.statusBar")} className="col-span-2">
          <StatusBar />
        </footer>
      </div>
      <RemoveTasksDialog
        request={removeRequest}
        onClose={() => setRemoveRequest(null)}
      />
      <ShortcutsDialog open={shortcutsOpen} onOpenChange={setShortcutsOpen} />
    </ShellActionsContext.Provider>
  );
}

function Placeholder({ screen }: { screen: string }) {
  const { t } = useTranslation();
  return <h1>{t(`screens.${screen}`)}</h1>;
}

function TasksRoute({
  filter,
  group,
}: {
  filter?: SidebarFilter;
  group?: "category" | "tag";
}) {
  const params = useParams();
  const shell = useContext(ShellActionsContext);
  const state = filter ?? params.filter ?? "all";
  const filters: SidebarFilter[] = [
    "all",
    "downloading",
    "completed",
    "active",
    "inactive",
    "stopped",
    "error",
  ];
  if (!filters.includes(state as SidebarFilter))
    return <Navigate to="/tasks/all" replace />;
  return (
    <TaskGrid
      filter={state as SidebarFilter}
      category={group === "category" ? (params.name ?? "") : undefined}
      tag={group === "tag" ? (params.name ?? "") : undefined}
      actions={shell ?? undefined}
    />
  );
}

export default function App() {
  const [queryClient] = useState(() => new QueryClient());
  return (
    <QueryClientProvider client={queryClient}>
      <I18nextProvider i18n={initI18n()}>
        <BrowserRouter basename={basePath()}>
          <SessionProvider>
            <Routes>
              <Route path="/setup" element={<AuthRoute screen="setup" />} />
              <Route path="/login" element={<AuthRoute screen="login" />} />
              <Route
                element={
                  <RequireAuth>
                    <AppLayout />
                  </RequireAuth>
                }
              >
                <Route path="/" element={<TasksRoute filter="all" />} />
                <Route path="/tasks/:filter" element={<TasksRoute />} />
                <Route
                  path="/tasks/category/:name"
                  element={<TasksRoute group="category" />}
                />
                <Route
                  path="/tasks/category"
                  element={<TasksRoute group="category" />}
                />
                <Route
                  path="/tasks/tag/:name"
                  element={<TasksRoute group="tag" />}
                />
                <Route path="/tasks/tag" element={<TasksRoute group="tag" />} />
                <Route
                  path="/search"
                  element={<Placeholder screen="search" />}
                />
                <Route
                  path="/rss/feeds"
                  element={<Placeholder screen="rss-feeds" />}
                />
                <Route
                  path="/rss/rules"
                  element={<Placeholder screen="rss-rules" />}
                />
                <Route
                  path="/settings/:section"
                  element={<Placeholder screen="settings" />}
                />
                <Route path="/logs" element={<Placeholder screen="logs" />} />
                <Route path="*" element={<Placeholder screen="tasks" />} />
              </Route>
            </Routes>
          </SessionProvider>
          <Toaster theme={readStoredTheme()} />
        </BrowserRouter>
      </I18nextProvider>
    </QueryClientProvider>
  );
}
