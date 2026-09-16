import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api/client";
import { useAuthActions } from "../../App";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";

const tooManyRequests = 429;

export function LoginScreen() {
  const { t } = useTranslation();
  const { authenticate } = useAuthActions();
  const [pending, setPending] = useState(false);
  const [message, setMessage] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending) return;
    const fields = new FormData(event.currentTarget);
    setPending(true);
    setMessage("");
    try {
      const { data, error, response } = await api.POST("/auth/login", {
        body: {
          username: String(fields.get("username")),
          password: String(fields.get("password")),
        },
      });
      if (data) {
        // No imperative navigate here: BrowserRouter routes location updates
        // through startTransition while the session notification is deferred,
        // so a transition render can read the stale committed session and
        // bounce the route back to /login?next=. The AuthRoute gate owns the
        // redirect (it honors next); it only navigates after the authenticated
        // session has committed.
        authenticate(data);
        return;
      }
      if (response.status === tooManyRequests) {
        setMessage(
          t("auth.retryAfter", {
            seconds: response.headers.get("Retry-After"),
          }),
        );
        return;
      }
      // The server intentionally uses the same detail for every invalid credential.
      setMessage(error?.detail ?? t("auth.requestError"));
    } catch {
      setMessage(t("auth.connectionError"));
    } finally {
      setPending(false);
    }
  }

  return (
    <section
      className="mx-auto flex max-w-sm flex-col gap-4 p-6"
      aria-labelledby="login-title"
    >
      <h1 id="login-title">{t("auth.signIn")}</h1>
      <form
        onSubmit={(event) => void submit(event)}
        className="flex flex-col gap-3"
      >
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
        {message && (
          <p role="alert">{t("auth.serverDetail", { detail: message })}</p>
        )}
        <Button type="submit" disabled={pending}>
          {t("auth.signIn")}
        </Button>
      </form>
    </section>
  );
}
