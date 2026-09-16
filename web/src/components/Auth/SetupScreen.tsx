import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import { useAuthActions } from "../../App";
import { DEFAULT_LOCALE } from "../../i18n";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

const minimumPasswordLength = 12;
const conflict = 409;
const tooManyRequests = 429;

export function SetupScreen() {
  const { t } = useTranslation();
  const { authenticate, setupComplete } = useAuthActions();
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState("");
  const validPassword =
    [...password].length >= minimumPasswordLength && password === confirmation;

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || !validPassword) return;
    const fields = new FormData(event.currentTarget);
    setPending(true);
    setMessage("");
    try {
      const { data, error, response } = await api.POST("/auth/setup", {
        body: {
          setup_token: String(fields.get("setup_token")),
          username: String(fields.get("username")),
          password,
          locale: String(fields.get("locale")),
        },
      });
      if (data) {
        // The AuthRoute gate owns the redirect; see LoginScreen for why the
        // submit handler must not navigate ahead of the committed session.
        authenticate(data);
        return;
      }
      if (
        response.status === conflict &&
        error?.type === "/problems/setup-already-complete"
      ) {
        // Another browser may finish setup while this form is open.
        setupComplete();
        toast.info(t("auth.setupComplete"));
        return;
      }
      setMessage(
        response.status === tooManyRequests
          ? t("auth.retryAfter", {
              seconds: response.headers.get("Retry-After"),
            })
          : (error?.detail ?? t("auth.requestError")),
      );
    } catch {
      setMessage(t("auth.connectionError"));
    } finally {
      setPending(false);
    }
  }

  return (
    <section
      className="mx-auto flex max-w-sm flex-col gap-4 p-6"
      aria-labelledby="setup-title"
    >
      <h1 id="setup-title">{t("auth.setupTitle")}</h1>
      <form
        onSubmit={(event) => void submit(event)}
        className="flex flex-col gap-3"
      >
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
          aria-describedby="password-requirement"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          required
        />
        <p id="password-requirement">
          {t("auth.passwordRequirement", { count: minimumPasswordLength })}
        </p>
        <Label htmlFor="confirmation">{t("auth.confirmPassword")}</Label>
        <Input
          id="confirmation"
          type="password"
          autoComplete="new-password"
          value={confirmation}
          onChange={(event) => setConfirmation(event.target.value)}
          required
        />
        {confirmation && password !== confirmation && (
          <p role="alert">{t("auth.passwordMismatch")}</p>
        )}
        <Label htmlFor="locale">{t("auth.locale")}</Label>
        <select id="locale" name="locale" defaultValue={DEFAULT_LOCALE}>
          <option value={DEFAULT_LOCALE}>{t("auth.english")}</option>
        </select>
        {message && (
          <p role="alert">{t("auth.serverDetail", { detail: message })}</p>
        )}
        <Button type="submit" disabled={pending || !validPassword}>
          {t("auth.createAccount")}
        </Button>
      </form>
    </section>
  );
}
