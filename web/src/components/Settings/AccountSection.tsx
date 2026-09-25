import { useCallback, useEffect, useRef, useState, type JSX } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatAbsolute, formatWhen } from "../../lib/format";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { useSettingsDirty } from "./SettingsScreen";

initI18n().addResourceBundle("en", "settings", settingsStrings);

type Problem = components["schemas"]["ErrorModel"];
type PatchAccountBody = components["schemas"]["PatchAccountInputBody"];
type CreateTokenBody = components["schemas"]["CreateTokenInputBody"];

/** GET /account, doc 05 §12. There is no password member in any read shape. */
export interface Account {
  id: string;
  username: string;
  enabled: boolean;
  locale: string;
  last_login_at: string | null;
  created_at: string;
}

/** PATCH /account. `current_password` is required whenever `password` is
 *  present, and the call revokes every session except the caller's. API
 *  tokens are unaffected. */
export interface AccountPatch {
  username?: string;
  locale?: string;
  password?: string;
  current_password?: string;
}

/** One row of GET /api-tokens. Never carries the secret. */
export interface TokenRow {
  id: string;
  name: string;
  prefix: string;
  last_used_at: string | null;
  expires_at: string | null;
  created_at: string;
}

/** The create-once reveal. `token` comes only from the 201 body of
 *  POST /api-tokens and is held in component state alone: it is never
 *  written to prefs, localStorage, a query cache or a log, and the dialog
 *  cannot be reopened once closed. Closing it clears the state. */
export interface TokenRevealProps {
  token: string;
  onClose: () => void;
}

const ACCOUNT_KEY = ["account"] as const;
const TOKENS_KEY = ["api-tokens"] as const;

// Doc 05 §12's floor for a new password; the server re-checks it.
const PASSWORD_FLOOR = 12;

function problemDetail(error: Problem | undefined): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

function isForbidden(error: Problem | undefined): boolean {
  return error?.status === 403 || error?.type === "/problems/forbidden";
}

function isValidation(error: Problem | undefined): boolean {
  return error?.status === 422 || error?.type === "/problems/validation-failed";
}

function toAccount(dto: components["schemas"]["UserBody"]): Account {
  return {
    id: dto.id,
    username: dto.username,
    enabled: dto.enabled,
    locale: dto.locale,
    last_login_at: dto.last_login_at ?? null,
    created_at: dto.created_at,
  };
}

/** The one-shot reveal of doc 05 §12: the value renders inside the dialog,
 *  Copy hands it to the clipboard, and onClose must clear the holding state
 *  so the value cannot be shown again. */
export function TokenRevealDialog({
  token,
  onClose,
}: TokenRevealProps): JSX.Element {
  const { t } = useTranslation("settings");
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(token);
      setCopied(true);
    } catch {
      // No clipboard permission leaves the value on screen to copy by hand.
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent
        className="sm:max-w-md"
        aria-label={t("account.revealTitle")}
      >
        <DialogHeader>
          <DialogTitle>{t("account.revealTitle")}</DialogTitle>
          <DialogDescription>{t("account.revealWarning")}</DialogDescription>
        </DialogHeader>
        <code className="block break-all rounded-md border border-border bg-muted px-3 py-2 font-mono text-sm">
          {token}
        </code>
        <DialogFooter>
          <Button variant="outline" onClick={() => void copy()}>
            {copied ? t("account.revealCopied") : t("account.revealCopy")}
          </Button>
          <Button onClick={onClose}>{t("account.revealClose")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Editable profile state: the inputs' own text. */
interface ProfileForm {
  username: string;
  locale: string;
}

export function AccountSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();

  const account = useQuery({
    queryKey: ACCOUNT_KEY,
    queryFn: async (): Promise<Account> => {
      const { data, error } = await api.GET("/account");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return toAccount(data);
    },
    retry: false,
  });
  const tokens = useQuery({
    queryKey: TOKENS_KEY,
    queryFn: async (): Promise<TokenRow[]> => {
      const { data, error } = await api.GET("/api-tokens");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data.items ?? [];
    },
    retry: false,
  });

  const [form, setForm] = useState<ProfileForm | null>(null);
  const [baseline, setBaseline] = useState<ProfileForm | null>(null);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [currentPasswordError, setCurrentPasswordError] = useState<
    string | null
  >(null);
  const [newPasswordError, setNewPasswordError] = useState<string | null>(null);
  const [passwordChanged, setPasswordChanged] = useState(false);
  const [changingPassword, setChangingPassword] = useState(false);
  const [tokenName, setTokenName] = useState("");
  const [tokenExpires, setTokenExpires] = useState("");
  const [creatingToken, setCreatingToken] = useState(false);
  const [revealed, setRevealed] = useState<string | null>(null);
  const [revokeTarget, setRevokeTarget] = useState<TokenRow | null>(null);
  const savingRef = useRef(false);

  const formRef = useRef(form);
  const baselineRef = useRef(baseline);
  // Refs mirror state for the dirty-bar callbacks; writing them in an
  // effect instead of during render keeps an abandoned concurrent render
  // from leaving the mirror inconsistent with committed state.
  useEffect(() => {
    formRef.current = form;
    baselineRef.current = baseline;
  }, [form, baseline]);

  // The seed runs once, on the first render with the account; a later
  // refetch refreshes the query, not the in-progress form.
  useEffect(() => {
    if (account.data === undefined || form !== null) return;
    const seeded = {
      username: account.data.username,
      locale: account.data.locale,
    };
    setForm(seeded);
    setBaseline({ ...seeded });
  }, [account.data, form]);

  const dirtyCount =
    form === null || baseline === null
      ? 0
      : [
          form.username !== baseline.username,
          form.locale !== baseline.locale,
        ].filter(Boolean).length;

  /** One PATCH /account carrying only the changed profile members; a
   *  rejected call leaves the form dirty. */
  const save = useCallback(() => {
    const current = formRef.current;
    const base = baselineRef.current;
    if (current === null || base === null || savingRef.current) return;
    savingRef.current = true;
    void (async () => {
      try {
        const body: PatchAccountBody = {};
        if (current.username !== base.username)
          body.username = current.username;
        if (current.locale !== base.locale) body.locale = current.locale;
        const { data, error } = await api.PATCH("/account", { body });
        if (error !== undefined) {
          toast.error(
            t("account.saveFailed", {
              detail: problemDetail(error) ?? ct("shell.networkError"),
            }),
          );
          return;
        }
        const next = {
          username: data?.username ?? current.username,
          locale: data?.locale ?? current.locale,
        };
        // The baseline always advances to the server state, but the form
        // is only replaced when nothing was typed while the PATCH was in
        // flight — a new object identity means an edit happened.
        if (formRef.current === current) setForm(next);
        setBaseline({ ...next });
        await queryClient.invalidateQueries({ queryKey: ACCOUNT_KEY });
      } catch (error) {
        toast.error(
          t("account.saveFailed", {
            detail:
              error instanceof Error ? error.message : ct("shell.networkError"),
          }),
        );
      } finally {
        savingRef.current = false;
      }
    })();
  }, [queryClient, t, ct]);

  const revert = useCallback(() => {
    const base = baselineRef.current;
    setForm(base === null ? null : { ...base });
    void queryClient.invalidateQueries({ queryKey: ACCOUNT_KEY });
  }, [queryClient]);

  useEffect(() => {
    useSettingsDirty.setState((prev) => {
      if (dirtyCount === 0)
        return prev.report === null ? prev : { report: null };
      if (
        prev.report?.count === dirtyCount &&
        prev.report.section === "account"
      )
        return prev;
      return {
        report: { section: "account", count: dirtyCount, save, revert },
      };
    });
  }, [dirtyCount, save, revert]);
  useEffect(() => () => useSettingsDirty.setState({ report: null }), []);

  /** The password change is a discrete action, not a dirty-bar field: it
   *  sends both members of doc 05 §12 and maps 403 onto the
   *  current_password input and 422 onto the new-password input. */
  const changePassword = async () => {
    setCurrentPasswordError(null);
    setNewPasswordError(null);
    setPasswordChanged(false);
    if (newPassword.length < PASSWORD_FLOOR) {
      setNewPasswordError(t("account.passwordTooShort"));
      return;
    }
    setChangingPassword(true);
    try {
      const { error } = await api.PATCH("/account", {
        body: {
          password: newPassword,
          current_password: currentPassword,
        },
      });
      if (error !== undefined) {
        if (isForbidden(error))
          setCurrentPasswordError(t("account.passwordMismatch"));
        else if (isValidation(error))
          setNewPasswordError(
            problemDetail(error) ?? t("account.passwordTooShort"),
          );
        else
          toast.error(
            t("account.passwordChangeFailed", {
              detail: problemDetail(error) ?? ct("shell.networkError"),
            }),
          );
        return;
      }
      setPasswordChanged(true);
      setCurrentPassword("");
      setNewPassword("");
    } catch {
      toast.error(
        t("account.passwordChangeFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setChangingPassword(false);
    }
  };

  /** The 201 body's token member goes to component state alone — never a
   *  query cache, a storage or a log — and the reveal dialog clears it on
   *  close, so it cannot be shown twice. */
  const createToken = async () => {
    const name = tokenName.trim();
    if (name === "") return;
    setCreatingToken(true);
    try {
      const body: CreateTokenBody = { name, expires_at: null };
      if (tokenExpires !== "")
        body.expires_at = new Date(tokenExpires).toISOString();
      const { data, error } = await api.POST("/api-tokens", { body });
      if (data === undefined) {
        toast.error(
          t("account.tokenCreateFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
        return;
      }
      setRevealed(data.token);
      setTokenName("");
      setTokenExpires("");
      await tokens.refetch();
    } catch {
      toast.error(
        t("account.tokenCreateFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setCreatingToken(false);
    }
  };

  const revokeToken = async () => {
    const target = revokeTarget;
    if (target === null) return;
    // Close the dialog before the request so a double-click cannot fire a
    // second DELETE whose 404 would toast a failure after a success.
    setRevokeTarget(null);
    try {
      const { error } = await api.DELETE("/api-tokens/{id}", {
        params: { path: { id: target.id } },
      });
      if (error !== undefined) {
        toast.error(
          t("account.tokenRevokeFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
        return;
      }
    } catch {
      toast.error(
        t("account.tokenRevokeFailed", {
          detail: ct("shell.networkError"),
        }),
      );
      return;
    }
    await tokens.refetch();
  };

  // A failed background refetch keeps the previous data; only an error
  // with nothing cached replaces the whole section.
  if (account.isError && account.data === undefined)
    return (
      <p role="alert" className="text-sm text-destructive">
        {t("account.loadError")}{" "}
        <Button
          variant="outline"
          size="sm"
          onClick={() => void account.refetch()}
        >
          {ct("actions.retry")}
        </Button>
      </p>
    );
  if (form === null)
    return (
      <p className="text-sm text-muted-foreground">{t("account.loading")}</p>
    );

  const tokenRows = tokens.data ?? [];

  return (
    <div className="flex max-w-xl flex-col gap-6">
      <section aria-label={t("account.profileHeading")}>
        <h2 className="mb-2 text-sm font-semibold">
          {t("account.profileHeading")}
        </h2>
        <div className="flex flex-col gap-3">
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="account-username">{t("account.username")}</Label>
            <Input
              id="account-username"
              className="w-64"
              value={form.username}
              onChange={(event) =>
                setForm({ ...form, username: event.target.value })
              }
            />
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="account-locale">{t("account.locale")}</Label>
            <Input
              id="account-locale"
              className="w-64"
              value={form.locale}
              onChange={(event) =>
                setForm({ ...form, locale: event.target.value })
              }
            />
          </div>
          <div className="flex items-center justify-between gap-4 text-sm">
            <span className="text-muted-foreground">
              {t("account.lastLogin")}
            </span>
            <span>
              {account.data?.last_login_at == null ? (
                t("account.never")
              ) : (
                <span title={formatAbsolute(account.data.last_login_at)}>
                  {formatWhen(account.data.last_login_at)}
                </span>
              )}
            </span>
          </div>
          <div className="flex items-start justify-between gap-4 text-sm">
            <span className="pt-0.5 text-muted-foreground">
              {t("account.sessionLifetime")}
            </span>
            <p className="w-64 text-xs text-muted-foreground">
              {t("account.sessionLifetimeNote")}
            </p>
          </div>
        </div>
      </section>

      <section aria-label={t("account.passwordHeading")}>
        <h2 className="mb-2 text-sm font-semibold">
          {t("account.passwordHeading")}
        </h2>
        <div className="flex flex-col gap-3">
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="account-current-password">
              {t("account.currentPassword")}
            </Label>
            <Input
              id="account-current-password"
              type="password"
              autoComplete="current-password"
              className="w-64"
              value={currentPassword}
              aria-invalid={currentPasswordError !== null || undefined}
              aria-describedby={
                currentPasswordError !== null
                  ? "account-current-password-error"
                  : undefined
              }
              onChange={(event) => {
                setCurrentPassword(event.target.value);
                setCurrentPasswordError(null);
              }}
            />
          </div>
          {currentPasswordError !== null && (
            <p
              id="account-current-password-error"
              role="alert"
              className="text-xs text-destructive"
            >
              {currentPasswordError}
            </p>
          )}
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="account-new-password">
              {t("account.newPassword")}
            </Label>
            <Input
              id="account-new-password"
              type="password"
              autoComplete="new-password"
              className="w-64"
              value={newPassword}
              aria-invalid={newPasswordError !== null || undefined}
              aria-describedby={
                newPasswordError !== null
                  ? "account-new-password-error"
                  : "account-new-password-rule"
              }
              onChange={(event) => {
                setNewPassword(event.target.value);
                setNewPasswordError(null);
              }}
            />
          </div>
          {newPasswordError !== null ? (
            <p
              id="account-new-password-error"
              role="alert"
              className="text-xs text-destructive"
            >
              {newPasswordError}
            </p>
          ) : (
            <p
              id="account-new-password-rule"
              className="text-xs text-muted-foreground"
            >
              {t("account.passwordRule")}
            </p>
          )}
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              disabled={
                changingPassword || currentPassword === "" || newPassword === ""
              }
              onClick={() => void changePassword()}
            >
              {changingPassword
                ? t("account.changingPassword")
                : t("account.changePassword")}
            </Button>
            {passwordChanged && (
              <p role="status" className="text-xs text-muted-foreground">
                {t("account.passwordChanged")}
              </p>
            )}
          </div>
        </div>
      </section>

      <section aria-label={t("account.tokensHeading")}>
        <h2 className="mb-2 text-sm font-semibold">
          {t("account.tokensHeading")}
        </h2>
        <p className="mb-3 text-xs text-muted-foreground">
          {t("account.tokensNote")}
        </p>
        <div className="mb-3 flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <Label htmlFor="account-token-name">
              {t("account.tokenNameLabel")}
            </Label>
            <Input
              id="account-token-name"
              className="w-56"
              value={tokenName}
              onChange={(event) => setTokenName(event.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1">
            <Label htmlFor="account-token-expires">
              {t("account.tokenExpiresLabel")}
            </Label>
            <Input
              id="account-token-expires"
              type="datetime-local"
              className="w-56"
              value={tokenExpires}
              onChange={(event) => setTokenExpires(event.target.value)}
            />
          </div>
          <Button
            size="sm"
            disabled={creatingToken || tokenName.trim() === ""}
            onClick={() => void createToken()}
          >
            {t("account.tokenCreate")}
          </Button>
        </div>
        {tokens.isError && tokens.data === undefined ? (
          <p role="alert" className="text-sm text-destructive">
            {t("account.tokensLoadError")}{" "}
            <Button
              variant="outline"
              size="sm"
              onClick={() => void tokens.refetch()}
            >
              {ct("actions.retry")}
            </Button>
          </p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border text-start text-muted-foreground">
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColName")}
                </th>
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColPrefix")}
                </th>
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColLastUsed")}
                </th>
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColExpires")}
                </th>
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColCreated")}
                </th>
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("account.tokenColActions")}
                </th>
              </tr>
            </thead>
            <tbody>
              {tokens.isLoading ? (
                <tr>
                  <td colSpan={6} className="px-2 py-2 text-muted-foreground">
                    {t("account.tokensLoading")}
                  </td>
                </tr>
              ) : tokenRows.length === 0 ? (
                <tr>
                  <td colSpan={6} className="px-2 py-2 text-muted-foreground">
                    {t("account.tokenEmpty")}
                  </td>
                </tr>
              ) : (
                tokenRows.map((row) => (
                  <tr key={row.id} className="border-b border-border">
                    <td className="px-2 py-2 font-medium">{row.name}</td>
                    <td className="px-2 py-2 font-mono text-xs">
                      {row.prefix}
                    </td>
                    <td className="px-2 py-2">
                      {row.last_used_at === null ? (
                        t("account.never")
                      ) : (
                        <span title={formatAbsolute(row.last_used_at)}>
                          {formatWhen(row.last_used_at)}
                        </span>
                      )}
                    </td>
                    <td className="px-2 py-2">
                      {row.expires_at === null ? (
                        t("account.never")
                      ) : (
                        <span title={formatAbsolute(row.expires_at)}>
                          {formatWhen(row.expires_at)}
                        </span>
                      )}
                    </td>
                    <td className="px-2 py-2">
                      <span title={formatAbsolute(row.created_at)}>
                        {formatWhen(row.created_at)}
                      </span>
                    </td>
                    <td className="px-2 py-2">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setRevokeTarget(row)}
                      >
                        {t("account.tokenRevoke")}
                      </Button>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        )}
      </section>

      {revealed !== null && (
        <TokenRevealDialog token={revealed} onClose={() => setRevealed(null)} />
      )}

      {revokeTarget !== null && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setRevokeTarget(null);
          }}
        >
          <DialogContent
            className="sm:max-w-md"
            aria-label={t("account.tokenRevokeTitle")}
          >
            <DialogHeader>
              <DialogTitle>{t("account.tokenRevokeTitle")}</DialogTitle>
              <DialogDescription>
                {t("account.tokenRevokeBody", { name: revokeTarget.name })}
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setRevokeTarget(null)}>
                {t("account.tokenRevokeCancel")}
              </Button>
              <Button onClick={() => void revokeToken()}>
                {t("account.tokenRevokeConfirm")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
