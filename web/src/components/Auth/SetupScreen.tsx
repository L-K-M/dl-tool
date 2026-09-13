import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useNavigate } from "react-router-dom";
import { toast } from "sonner";
import { AUTH_STATUS, useSession, useSessionUpdate } from "../../App";
import { api } from "../../api/client";
import { DEFAULT_LOCALE } from "../../i18n";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

const MIN_PASSWORD_LENGTH = 12;

export function SetupScreen() {
  const { t } = useTranslation();
  const session = useSession();
  const updateSession = useSessionUpdate();
  const navigate = useNavigate();
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState("");
  const validPassword =
    [...password].length >= MIN_PASSWORD_LENGTH && password === confirmation;

  if (session.status === "authenticated") return <Navigate to="/" replace />;
  if (session.status === "anonymous") return <Navigate to="/login" replace />;

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || !validPassword) return;
    const form = new FormData(event.currentTarget);
    setPending(true);
    setMessage("");
    try {
      const { data, error, response } = await api.POST("/auth/setup", {
        body: {
          setup_token: String(form.get("setup_token")),
          username: String(form.get("username")),
          password,
          locale: String(form.get("locale")),
        },
      });
      if (data) {
        updateSession(data);
        await navigate("/", { replace: true });
        return;
      }
      // Another browser may have completed setup since our boot request.
      if (
        response.status === AUTH_STATUS.conflict &&
        error?.type === "/problems/setup-already-complete"
      ) {
        updateSession(null);
        toast.info(t("auth.setupComplete"));
        await navigate("/login", { replace: true });
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
      <h1>{t("auth.setup")}</h1>
      <form onSubmit={(event) => void submit(event)} className="grid gap-3">
        <Label htmlFor="setup-token">{t("auth.setupToken")}</Label>
        <Input
          id="setup-token"
          name="setup_token"
          type="password"
          autoComplete="off"
          required
        />
        <Label htmlFor="username">{t("auth.username")}</Label>
        <Input id="username" name="username" autoComplete="username" required />
        <Label htmlFor="password">{t("auth.password")}</Label>
        <Input
          id="password"
          name="password"
          type="password"
          autoComplete="new-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          aria-describedby="password-requirement"
        />
        <p id="password-requirement">
          {t("auth.passwordRequirement", { count: MIN_PASSWORD_LENGTH })}
        </p>
        <Label htmlFor="confirmation">{t("auth.confirmPassword")}</Label>
        <Input
          id="confirmation"
          name="confirmation"
          type="password"
          autoComplete="new-password"
          required
          value={confirmation}
          onChange={(event) => setConfirmation(event.target.value)}
          aria-describedby="password-match"
        />
        {confirmation && password !== confirmation && (
          <p id="password-match">{t("auth.passwordMismatch")}</p>
        )}
        <Label htmlFor="locale">{t("auth.locale")}</Label>
        <select id="locale" name="locale" defaultValue={DEFAULT_LOCALE}>
          <option value={DEFAULT_LOCALE}>{t("auth.english")}</option>
        </select>
        {message && <p role="alert">{message}</p>}
        <Button type="submit" disabled={pending || !validPassword}>
          {t("auth.createAccount")}
        </Button>
      </form>
    </main>
  );
}
