import {
  createContext,
  useContext,
  useRef,
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
import { toast } from "sonner";
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
import { api, basePath, setCsrfToken } from "./api/client";
import type { components } from "./api/schema";
import { LoginScreen } from "./components/Auth/LoginScreen";
import { SetupScreen } from "./components/Auth/SetupScreen";
import {
  RemoveConfirmDialog,
  ShortcutSheet,
  Toolbar,
  useNameFilter,
} from "./components/Shell/Toolbar";
import { Sidebar } from "./components/Shell/Sidebar";
import { StatusBar } from "./components/Shell/StatusBar";
import { Toaster } from "./components/ui/sonner";
import { initI18n } from "./i18n";
import { readStoredTheme } from "./lib/theme";
import { TaskGrid, invalidateTaskList } from "./components/TaskGrid/TaskGrid";
import { useTasks, type SidebarFilter } from "./store/useTasks";

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
    signOut: async () => {
      await api.POST("/auth/logout").catch(() => undefined);
      setCsrfToken(null);
      queryClient.setQueryData<SessionState>(sessionKey, {
        status: "anonymous",
      });
      useTasks.getState().reset();
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

/** The §3.6 shell actions the grid consumes; AppLayout is their only provider. */
export interface ShellActions {
  removeSelected: (withData: boolean) => void;
  focusFilter: () => void;
  toggleShortcuts: () => void;
}
const ShellActionsContext = createContext<ShellActions | null>(null);

export function useShellActions(): ShellActions | null {
  return useContext(ShellActionsContext);
}

export function AppLayout() {
  const { t } = useTranslation();
  const session = useSession();
  const { signOut } = useAuthActions();
  const queryClient = useQueryClient();
  const filterInputRef = useRef<HTMLInputElement>(null);
  // Refocused when either dialog closes, so focus returns without touching selection.
  const restoreFocus = useRef<HTMLElement | null>(null);
  const [remove, setRemove] = useState<{ withData: boolean } | null>(null);
  const [removing, setRemoving] = useState(false);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  const selection = useTasks((state) => state.selection);

  const openRemove = (withData: boolean) => {
    // One dialog at a time; repeats while open are ignored, and an empty
    // selection has nothing to confirm.
    if (remove !== null || useTasks.getState().selection.size === 0) return;
    restoreFocus.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    setRemove({ withData });
  };

  const confirmRemove = async (deleteData: boolean) => {
    const ids = [...useTasks.getState().selection];
    setRemoving(true);
    const failed: string[] = [];
    for (const id of ids) {
      const { error } = await api.DELETE("/tasks/{id}", {
        params: { path: { id }, query: { delete_data: deleteData } },
      });
      if (error) failed.push(error.detail ?? id);
    }
    setRemoving(false);
    setRemove(null);
    if (failed.length > 0) {
      toast.error(t("removeDialog.failed", { count: failed.length }));
      return;
    }
    useTasks.getState().clearSelection();
    toast.success(t("removeDialog.removed", { count: ids.length }));
    void invalidateTaskList(queryClient);
  };

  const shellActions: ShellActions = {
    removeSelected: openRemove,
    focusFilter: () => filterInputRef.current?.focus(),
    toggleShortcuts: () => setShortcutsOpen((open) => !open),
  };

  return (
    <ShellActionsContext.Provider value={shellActions}>
      <div
        data-testid="app-root"
        className="grid h-dvh grid-cols-[220px_minmax(0,1fr)] grid-rows-[48px_minmax(0,1fr)_28px] overflow-hidden"
      >
        <header aria-label={t("regions.header")} className="col-span-2">
          <Toolbar
            onRemove={() => openRemove(false)}
            filterInputRef={filterInputRef}
            user={session.status === "authenticated" ? session.user : null}
            onSignOut={() => void signOut()}
          />
        </header>
        <aside aria-label={t("regions.sidebar")} className="overflow-y-auto">
          <Sidebar />
        </aside>
        <main className="min-w-0 overflow-hidden">
          <Outlet />
        </main>
        <footer aria-label={t("regions.statusBar")} className="col-span-2">
          <StatusBar />
        </footer>
        <RemoveConfirmDialog
          open={remove !== null}
          ids={[...selection]}
          preDeleteData={remove?.withData ?? false}
          busy={removing}
          onCancel={() => setRemove(null)}
          onConfirm={(deleteData) => void confirmRemove(deleteData)}
          restoreFocus={restoreFocus}
        />
        <ShortcutSheet
          open={shortcutsOpen}
          onClose={() => setShortcutsOpen(false)}
          restoreFocus={restoreFocus}
        />
      </div>
    </ShellActionsContext.Provider>
  );
}

function Placeholder({ screen }: { screen: string }) {
  const { t } = useTranslation();
  return <h1>{t(`screens.${screen}`)}</h1>;
}

export function TasksRoute({
  filter,
  group,
}: {
  filter?: SidebarFilter;
  group?: "category" | "tag";
}) {
  const params = useParams();
  const shell = useShellActions();
  const nameFilter = useNameFilter();
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
      // An absent :name is the Uncategorised/Untagged pseudo-node; the API's
      // empty value selects those tasks.
      category={group === "category" ? (params.name ?? "") : undefined}
      tag={group === "tag" ? (params.name ?? "") : undefined}
      nameQuery={nameFilter.value}
      onRemoveSelected={shell?.removeSelected}
      onFocusFilter={shell?.focusFilter}
      onShowShortcuts={shell?.toggleShortcuts}
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
                  path="/tasks/category/:name?"
                  element={<TasksRoute group="category" />}
                />
                <Route
                  path="/tasks/tag/:name?"
                  element={<TasksRoute group="tag" />}
                />
                <Route
                  path="/search"
                  element={<Placeholder screen="search" />}
                />
                <Route
                  path="/search/saved"
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
