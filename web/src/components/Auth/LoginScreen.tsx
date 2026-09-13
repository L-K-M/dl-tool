import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { AUTH_STATUS, safeNext, useSession, useSessionUpdate } from "../../App";
import { api } from "../../api/client";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

export function LoginScreen() {
  const { t } = useTranslation();
  const session = useSession();
  const updateSession = useSessionUpdate();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState("");

  if (session.status === "setup-required")
    return <Navigate to="/setup" replace />;
  if (session.status === "authenticated") return <Navigate to="/" replace />;

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    const form = new FormData(event.currentTarget);
    setPending(true);
    setMessage("");
    try {
      const { data, error, response } = await api.POST("/auth/login", {
        body: {
          username: String(form.get("username")),
          password: String(form.get("password")),
        },
      });
      if (data) {
        updateSession(data);
        await navigate(safeNext(params.get("next")), { replace: true });
        return;
      }
      setMessage(
        response.status === AUTH_STATUS.rateLimited
          ? t("auth.rateLimited", {
              seconds:
                response.headers.get("Retry-After") ?? t("auth.unknownWait"),
            })
          : t("auth.serverDetail", {
              detail: error?.detail ?? t("auth.unavailable"),
            }),
      );
    } catch {
      setMessage(t("auth.unavailable"));
    } finally {
      setPending(false);
    }
  }

  return (
    <main className="mx-auto max-w-sm p-6">
      <h1>{t("auth.login")}</h1>
      <form onSubmit={(event) => void submit(event)} className="grid gap-3">
        <Label htmlFor="username">{t("auth.username")}</Label>
        <Input id="username" name="username" autoComplete="username" required />
        <Label htmlFor="password">{t("auth.password")}</Label>
        <Input
          id="password"
          name="password"
          type="password"
          autoComplete="current-password"
          required
        />
        {message && <p role="alert">{message}</p>}
        <Button type="submit" disabled={pending}>
          {t("auth.login")}
        </Button>
      </form>
    </main>
  );
}
