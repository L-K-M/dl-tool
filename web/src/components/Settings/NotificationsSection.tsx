import { useRef, useState, type JSX } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { api } from "../../api/client";
import type { components } from "../../api/schema";
import { initI18n } from "../../i18n";
import { formatAbsolute, formatWhen } from "../../lib/format";
import settingsStrings from "../../locales/en/settings.json";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
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

initI18n().addResourceBundle("en", "settings", settingsStrings);

type Problem = components["schemas"]["ErrorModel"];
type ChannelDTO = components["schemas"]["ChannelView"];
type WriteBody = components["schemas"]["ChannelWriteBody"];

type ChannelKind = ChannelDTO["kind"];

/** One row of GET /notifications, doc 05 §14. `secret` is never a member of
 *  a read shape — `secret_set` reports whether one is stored. */
export interface ChannelRow {
  id: string;
  kind: ChannelKind;
  name: string;
  enabled: boolean;
  config: Record<string, unknown>;
  secret_set: boolean;
  event_mask: string[];
  last_send_at: string | null;
  last_error: string | null;
}

/** POST /notifications/{id}/test, doc 05 §14.1: the raw upstream reply,
 *  unparsed. response is null on a transport failure and error carries the
 *  reason; the generated schema types response non-nullable, so the wire
 *  null is mapped here rather than trusted away. */
interface TestReply {
  ok: boolean;
  elapsed_ms: number;
  request: { method: string; url: string };
  response: {
    status_line: string;
    status: number;
    headers: Record<string, string>;
    body: string;
  } | null;
  error: string | null;
}

/** The task_events.code vocabulary the matrix renders, doc 04 §4.9 /
 *  doc 05 §14. Codes the server accepts but this list does not know are
 *  preserved verbatim across saves and are never rendered as a box. */
export const NOTIFIABLE_EVENTS = [
  "task.created",
  "task.completed",
  "task.error",
  "task.paused",
  "task.resumed",
  "task.removed",
  "task.data_deleted",
  "task.force_completed",
  "engine.rejected",
  "engine.unavailable",
  "postprocess.extract.completed",
  "postprocess.extract.failed",
  "postprocess.move.failed",
  "postprocess.hook.failed",
] as const;

const NOTIFIABLE_SET: ReadonlySet<string> = new Set(NOTIFIABLE_EVENTS);

const CHANNELS_KEY = ["notification-channels"] as const;

const selectClass =
  "h-8 w-full rounded-lg border border-input bg-transparent px-2 text-sm focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 outline-none";

function problemDetail(error: Problem | undefined): string | undefined {
  return error?.detail ?? error?.title ?? error?.type;
}

function toRow(dto: ChannelDTO): ChannelRow {
  return { ...dto, event_mask: dto.event_mask ?? [] };
}

/** The mask split: "*" means every event; the known codes drive the matrix
 *  boxes; anything else is carried through a save unchanged so a code added
 *  to the API later is never silently dropped. */
function splitMask(mask: string[]): {
  all: boolean;
  known: ReadonlySet<string>;
  extras: string[];
} {
  return {
    all: mask.includes("*"),
    known: new Set(mask.filter((code) => NOTIFIABLE_SET.has(code))),
    extras: mask.filter((code) => code !== "*" && !NOTIFIABLE_SET.has(code)),
  };
}

/** Rebuilds a mask in the documented event order, extras appended last. */
function maskForSave(known: ReadonlySet<string>, extras: string[]): string[] {
  return [...NOTIFIABLE_EVENTS.filter((code) => known.has(code)), ...extras];
}

type FieldKind = "text" | "int" | "list" | "headers" | "template" | "urls";

interface FieldSpec {
  key: string;
  kind: FieldKind;
}

/** The fixed config key set of doc 04 §4.8 per kind — the same table the
 *  API validates against; an unknown key is a 422. */
const KIND_FIELDS: Record<ChannelKind, FieldSpec[]> = {
  webhook: [
    { key: "url", kind: "text" },
    { key: "method", kind: "text" },
    { key: "headers", kind: "headers" },
    { key: "body_template", kind: "template" },
  ],
  ntfy: [
    { key: "server_url", kind: "text" },
    { key: "topic", kind: "text" },
    { key: "priority", kind: "int" },
    { key: "tags", kind: "list" },
    { key: "click_url", kind: "text" },
  ],
  gotify: [
    { key: "server_url", kind: "text" },
    { key: "priority", kind: "int" },
  ],
  apprise: [
    { key: "base_url", kind: "text" },
    { key: "config_key", kind: "text" },
    { key: "urls", kind: "urls" },
    { key: "tag", kind: "text" },
    { key: "type", kind: "text" },
    { key: "format", kind: "text" },
  ],
};

/** Which config key renders a hint line under its input, by field name. */
const FIELD_HINTS: Partial<Record<string, string>> = {
  headers: "headersHint",
  body_template: "body_templateHint",
  tags: "listHint",
  urls: "urlsHint",
};

/** Stored config value → editable text. Lists render comma-separated,
 *  headers and url lists one entry per line. */
function configToText(kind: FieldKind, value: unknown): string {
  if (value === null || value === undefined) return "";
  if (kind === "headers" && typeof value === "object" && !Array.isArray(value))
    return Object.entries(value as Record<string, unknown>)
      .map(([name, item]) => `${name}: ${String(item)}`)
      .join("\n");
  if (Array.isArray(value))
    return value.map(String).join(kind === "urls" ? "\n" : ", ");
  return String(value);
}

/** Editable text → the wire value. An empty field is omitted rather than
 *  sent as "" so required keys produce a 422, never an empty stand-in. */
function textToConfig(kind: FieldKind, raw: string): unknown {
  const trimmed = raw.trim();
  if (trimmed === "") return undefined;
  switch (kind) {
    case "int": {
      const parsed = Number(trimmed);
      return Number.isInteger(parsed) ? parsed : trimmed;
    }
    case "list":
      return trimmed
        .split(",")
        .map((item) => item.trim())
        .filter((item) => item !== "");
    case "urls":
      return trimmed
        .split("\n")
        .map((line) => line.trim())
        .filter((line) => line !== "");
    case "headers": {
      const headers: Record<string, string> = {};
      for (const line of trimmed.split("\n")) {
        if (line.trim() === "") continue;
        const colon = line.indexOf(":");
        if (colon < 0) headers[line.trim()] = "";
        else
          headers[line.slice(0, colon).trim()] = line.slice(colon + 1).trim();
      }
      return headers;
    }
    default:
      return trimmed;
  }
}

type ChannelDialogState =
  { mode: "add" } | { mode: "edit"; channel: ChannelRow };

function ChannelDialog({
  state,
  onClose,
}: {
  state: ChannelDialogState;
  onClose: (changed: boolean) => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const editing = state.mode === "edit" ? state.channel : null;
  const [name, setName] = useState(editing?.name ?? "");
  const [kind, setKind] = useState<ChannelKind>(editing?.kind ?? "webhook");
  const [enabled, setEnabled] = useState(editing?.enabled ?? true);
  const [fields, setFields] = useState<Record<string, string>>(() => {
    const seeded: Record<string, string> = {};
    if (editing !== null)
      for (const spec of KIND_FIELDS[editing.kind])
        seeded[spec.key] = configToText(spec.kind, editing.config[spec.key]);
    return seeded;
  });
  // Write-only and always starts empty, even when secret_set: the stored
  // value is never returned, so it is sent only when the user types a new
  // one; the clear box sends null and the marker text is never a write.
  const [secret, setSecret] = useState("");
  const [clearSecret, setClearSecret] = useState(false);
  const [busy, setBusy] = useState(false);

  const pickKind = (next: ChannelKind) => {
    setKind(next);
    // Keys are per kind; text typed under one kind is not a valid config
    // under another, so switching kinds starts the field set over.
    setFields({});
  };

  const buildConfig = (): Record<string, unknown> => {
    const config: Record<string, unknown> = {};
    for (const spec of KIND_FIELDS[kind]) {
      const value = textToConfig(spec.kind, fields[spec.key] ?? "");
      if (value !== undefined) config[spec.key] = value;
    }
    return config;
  };

  const report = (error: Problem | undefined) =>
    toast.error(
      t("notifications.dialog.saveFailed", {
        detail: problemDetail(error) ?? ct("shell.networkError"),
      }),
    );

  const submit = async () => {
    setBusy(true);
    try {
      const config = buildConfig();
      const body: WriteBody = { name: name.trim(), enabled, config };
      if (state.mode === "add") body.kind = kind;
      // secret is tri-state on the wire: absent keeps the stored value,
      // null clears it, a string replaces it. The redaction marker is never
      // sent — an operator who literally types it gets a real replace, but
      // the UI itself only ever writes what was typed.
      if (clearSecret) body.secret = null;
      else if (secret !== "") body.secret = secret;
      const { error } =
        state.mode === "add"
          ? await api.POST("/notifications", { body })
          : await api.PATCH("/notifications/{id}", {
              params: { path: { id: state.channel.id } },
              body,
            });
      if (error !== undefined) {
        report(error);
        return;
      }
      onClose(true);
    } catch {
      toast.error(
        t("notifications.dialog.saveFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setBusy(false);
    }
  };

  const title =
    state.mode === "add"
      ? t("notifications.dialog.addTitle")
      : t("notifications.dialog.editTitle");

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose(false);
      }}
    >
      <DialogContent className="sm:max-w-lg" aria-label={title}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="flex max-h-[70vh] flex-col gap-3 overflow-y-auto pe-1">
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="channel-name">
              {t("notifications.dialog.name")}
            </Label>
            <Input
              id="channel-name"
              className="w-64"
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </div>
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="channel-kind">
              {t("notifications.dialog.kind")}
            </Label>
            <select
              id="channel-kind"
              className={selectClass + " w-64"}
              value={kind}
              disabled={state.mode === "edit"}
              aria-describedby={
                state.mode === "edit" ? "channel-kind-note" : undefined
              }
              onChange={(event) => pickKind(event.target.value as ChannelKind)}
            >
              {(["webhook", "ntfy", "gotify", "apprise"] as const).map(
                (option) => (
                  <option key={option} value={option}>
                    {t(`notifications.kinds.${option}`)}
                  </option>
                ),
              )}
            </select>
          </div>
          {state.mode === "edit" && (
            <p id="channel-kind-note" className="text-xs text-muted-foreground">
              {t("notifications.dialog.kindImmutable")}
            </p>
          )}
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="channel-enabled">
              {t("notifications.dialog.enabled")}
            </Label>
            <Checkbox
              id="channel-enabled"
              checked={enabled}
              onCheckedChange={(checked) => setEnabled(checked === true)}
            />
          </div>
          {KIND_FIELDS[kind].map((spec) => {
            const id = `channel-field-${spec.key}`;
            const hintKey = FIELD_HINTS[spec.key];
            const hint =
              hintKey === undefined
                ? ""
                : t(`notifications.dialog.fields.${hintKey}`);
            const value = fields[spec.key] ?? "";
            const wide = spec.kind === "text" || spec.kind === "int";
            return (
              <div key={spec.key} className="flex flex-col gap-1">
                <div className="flex items-center justify-between gap-4">
                  <Label htmlFor={id}>
                    {t(`notifications.dialog.fields.${spec.key}`)}
                  </Label>
                  {wide ? (
                    <Input
                      id={id}
                      type={spec.kind === "int" ? "number" : "text"}
                      className="w-64"
                      value={value}
                      onChange={(event) =>
                        setFields((current) => ({
                          ...current,
                          [spec.key]: event.target.value,
                        }))
                      }
                    />
                  ) : (
                    <textarea
                      id={id}
                      rows={spec.kind === "template" ? 4 : 3}
                      className="w-64 rounded-lg border border-input bg-transparent px-2 py-1 font-mono text-xs focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 outline-none"
                      value={value}
                      onChange={(event) =>
                        setFields((current) => ({
                          ...current,
                          [spec.key]: event.target.value,
                        }))
                      }
                    />
                  )}
                </div>
                {hint !== "" && (
                  <p className="text-end text-xs text-muted-foreground">
                    {hint}
                  </p>
                )}
              </div>
            );
          })}
          <div className="flex items-center justify-between gap-4">
            <Label htmlFor="channel-secret">
              {t("notifications.dialog.secret")}
              {editing?.secret_set === true && (
                <span className="ms-1 text-xs text-muted-foreground">
                  {t("notifications.dialog.secretStored")}
                </span>
              )}
            </Label>
            <Input
              id="channel-secret"
              type="password"
              autoComplete="off"
              className="w-64"
              value={secret}
              disabled={clearSecret}
              onChange={(event) => setSecret(event.target.value)}
            />
          </div>
          {editing?.secret_set === true && (
            <>
              <div className="flex items-center justify-between gap-4">
                <Label htmlFor="channel-secret-clear">
                  {t("notifications.dialog.secretClear")}
                </Label>
                <Checkbox
                  id="channel-secret-clear"
                  checked={clearSecret}
                  onCheckedChange={(checked) =>
                    setClearSecret(checked === true)
                  }
                />
              </div>
              <p className="text-xs text-muted-foreground">
                {t("notifications.dialog.secretHint")}
              </p>
            </>
          )}
        </div>
        <DialogFooter>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => onClose(false)}
          >
            {t("notifications.dialog.cancel")}
          </Button>
          <Button
            disabled={busy || name.trim() === ""}
            onClick={() => void submit()}
          >
            {t("notifications.dialog.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function NotificationsSection(): JSX.Element {
  const { t } = useTranslation("settings");
  const { t: ct } = useTranslation();
  const queryClient = useQueryClient();
  const channels = useQuery({
    queryKey: CHANNELS_KEY,
    queryFn: async (): Promise<ChannelRow[]> => {
      const { data, error } = await api.GET("/notifications");
      if (data === undefined)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return (data.channels ?? []).map(toRow);
    },
    retry: false,
  });
  const rows = channels.data ?? [];
  const [dialog, setDialog] = useState<ChannelDialogState | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<ChannelRow | null>(null);
  const [results, setResults] = useState<Record<string, TestReply>>({});
  const [testing, setTesting] = useState<Record<string, boolean>>({});
  // The mask each channel had before its "All events" box was checked, so
  // unchecking restores exactly those codes rather than an empty mask.
  const rememberedMask = useRef<Record<string, string[]>>({});

  const patchMask = useMutation({
    mutationFn: async (change: { id: string; mask: string[] }) => {
      const { data, error } = await api.PATCH("/notifications/{id}", {
        params: { path: { id: change.id } },
        body: { event_mask: change.mask },
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data;
    },
    onMutate: async (change) => {
      await queryClient.cancelQueries({ queryKey: CHANNELS_KEY });
      const previous = queryClient.getQueryData<ChannelRow[]>(CHANNELS_KEY);
      queryClient.setQueryData<ChannelRow[]>(CHANNELS_KEY, (old) =>
        old?.map((row) =>
          row.id === change.id ? { ...row, event_mask: change.mask } : row,
        ),
      );
      return { previous };
    },
    onError: (error, change, context) => {
      if (context?.previous !== undefined)
        queryClient.setQueryData(CHANNELS_KEY, context.previous);
      const name = rows.find((row) => row.id === change.id)?.name ?? "";
      toast.error(
        t("notifications.maskSaveFailed", {
          name,
          detail: error.message,
        }),
      );
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: CHANNELS_KEY }),
  });

  const toggleEnabled = useMutation({
    mutationFn: async (change: { id: string; enabled: boolean }) => {
      const { data, error } = await api.PATCH("/notifications/{id}", {
        params: { path: { id: change.id } },
        body: { enabled: change.enabled },
      });
      if (error || !data)
        throw new Error(problemDetail(error) ?? ct("shell.networkError"));
      return data;
    },
    onMutate: async (change) => {
      await queryClient.cancelQueries({ queryKey: CHANNELS_KEY });
      const previous = queryClient.getQueryData<ChannelRow[]>(CHANNELS_KEY);
      queryClient.setQueryData<ChannelRow[]>(CHANNELS_KEY, (old) =>
        old?.map((row) =>
          row.id === change.id ? { ...row, enabled: change.enabled } : row,
        ),
      );
      return { previous };
    },
    onError: (error, _change, context) => {
      if (context?.previous !== undefined)
        queryClient.setQueryData(CHANNELS_KEY, context.previous);
      toast.error(t("notifications.enableFailed", { detail: error.message }));
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: CHANNELS_KEY }),
  });

  /** Every box PATCHes /notifications/{id} with the resulting event_mask.
   *  "All events" collapses the mask to ["*"] and remembers the boxes it
   *  replaced; unchecking restores them. */
  const setEvent = (row: ChannelRow, code: string, on: boolean) => {
    const { all, known, extras } = splitMask(row.event_mask);
    if (all) return;
    const next = new Set(known);
    if (on) next.add(code);
    else next.delete(code);
    patchMask.mutate({ id: row.id, mask: maskForSave(next, extras) });
  };

  const setAllEvents = (row: ChannelRow, on: boolean) => {
    if (on) {
      rememberedMask.current[row.id] = row.event_mask;
      patchMask.mutate({ id: row.id, mask: ["*"] });
    } else {
      const remembered = rememberedMask.current[row.id];
      delete rememberedMask.current[row.id];
      patchMask.mutate({ id: row.id, mask: remembered ?? [] });
    }
  };

  /** One test send per click; the 200 body — upstream answered or transport
   *  error — renders verbatim under the row, never summarised. */
  const sendTest = async (id: string) => {
    setTesting((current) => ({ ...current, [id]: true }));
    try {
      const { data, error } = await api.POST("/notifications/{id}/test", {
        params: { path: { id } },
        body: {},
      });
      if (data !== undefined)
        setResults((current) => ({
          ...current,
          [id]: data as unknown as TestReply,
        }));
      else
        toast.error(
          t("notifications.testFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
    } catch {
      toast.error(
        t("notifications.testFailed", {
          detail: ct("shell.networkError"),
        }),
      );
    } finally {
      setTesting((current) => ({ ...current, [id]: false }));
    }
  };

  const deleteChannel = async () => {
    const target = deleteTarget;
    if (target === null) return;
    try {
      const { error } = await api.DELETE("/notifications/{id}", {
        params: { path: { id: target.id } },
      });
      if (error !== undefined) {
        toast.error(
          t("notifications.deleteFailed", {
            detail: problemDetail(error) ?? ct("shell.networkError"),
          }),
        );
        return;
      }
    } catch {
      toast.error(
        t("notifications.deleteFailed", {
          detail: ct("shell.networkError"),
        }),
      );
      return;
    }
    setDeleteTarget(null);
    setResults((current) => {
      const next = { ...current };
      delete next[target.id];
      return next;
    });
    void channels.refetch();
  };

  const loadError = (
    <p role="alert" className="text-sm text-destructive">
      {t("notifications.loadError")}{" "}
      <Button
        variant="outline"
        size="sm"
        onClick={() => void channels.refetch()}
      >
        {ct("actions.retry")}
      </Button>
    </p>
  );

  // A failed background refetch keeps the previous data; only an error with
  // nothing cached replaces the whole section.
  if (channels.isError && channels.data === undefined) return loadError;

  const patching = (id: string) =>
    patchMask.isPending && patchMask.variables?.id === id;

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-2">
        <h2 className="me-auto text-sm font-semibold">
          {t("notifications.channelsHeading")}
        </h2>
        <Button size="sm" onClick={() => setDialog({ mode: "add" })}>
          {t("notifications.add")}
        </Button>
      </div>
      {channels.isError && loadError}
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b border-border text-start text-muted-foreground">
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("notifications.colName")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("notifications.colKind")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("notifications.colEnabled")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("notifications.colLastSend")}
            </th>
            <th scope="col" className="px-2 py-1.5 text-start font-medium">
              {t("notifications.colActions")}
            </th>
          </tr>
        </thead>
        <tbody>
          {channels.isLoading ? (
            <tr>
              <td colSpan={5} className="px-2 py-2 text-muted-foreground">
                {t("notifications.loading")}
              </td>
            </tr>
          ) : rows.length === 0 ? (
            <tr>
              <td colSpan={5} className="px-2 py-2 text-muted-foreground">
                {t("notifications.empty")}
              </td>
            </tr>
          ) : (
            rows.map((row) => {
              const result = results[row.id];
              const toggling =
                toggleEnabled.isPending &&
                toggleEnabled.variables?.id === row.id;
              return (
                <FragmentRow
                  key={row.id}
                  row={row}
                  result={result}
                  toggling={toggling}
                  sending={testing[row.id] === true}
                  onToggle={(enabled) =>
                    toggleEnabled.mutate({ id: row.id, enabled })
                  }
                  onTest={() => void sendTest(row.id)}
                  onEdit={() => setDialog({ mode: "edit", channel: row })}
                  onDelete={() => setDeleteTarget(row)}
                />
              );
            })
          )}
        </tbody>
      </table>

      {rows.length > 0 && (
        <>
          <h2 className="text-sm font-semibold">
            {t("notifications.matrixHeading")}
          </h2>
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border text-start text-muted-foreground">
                <th scope="col" className="px-2 py-1.5 text-start font-medium">
                  {t("notifications.matrixEventColumn")}
                </th>
                {rows.map((row) => (
                  <th
                    key={row.id}
                    scope="col"
                    className="px-2 py-1.5 text-start font-medium"
                  >
                    {row.name}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              <tr className="border-b border-border">
                <th scope="row" className="px-2 py-2 text-start font-medium">
                  {t("notifications.allEvents")}
                </th>
                {rows.map((row) => {
                  const { all } = splitMask(row.event_mask);
                  return (
                    <td key={row.id} className="px-2 py-2">
                      <Checkbox
                        checked={all}
                        disabled={patching(row.id)}
                        aria-label={t("notifications.allEventsFor", {
                          name: row.name,
                        })}
                        onCheckedChange={(checked) =>
                          setAllEvents(row, checked === true)
                        }
                      />
                    </td>
                  );
                })}
              </tr>
              {NOTIFIABLE_EVENTS.map((code) => (
                <tr key={code} className="border-b border-border">
                  <th scope="row" className="px-2 py-2 text-start font-normal">
                    {t(`notifications.events.${code}`)}
                  </th>
                  {rows.map((row) => {
                    const { all, known } = splitMask(row.event_mask);
                    return (
                      <td key={row.id} className="px-2 py-2">
                        <Checkbox
                          checked={all || known.has(code)}
                          disabled={all || patching(row.id)}
                          aria-label={t("notifications.eventFor", {
                            event: t(`notifications.events.${code}`),
                            name: row.name,
                          })}
                          onCheckedChange={(checked) =>
                            setEvent(row, code, checked === true)
                          }
                        />
                      </td>
                    );
                  })}
                </tr>
              ))}
              {/* A stored code outside NOTIFIABLE_EVENTS is an extra
               *  read-only row: the matrix cannot edit what it does not
               *  know, but the entry is preserved verbatim across saves. */}
              {[
                ...new Set(
                  rows.flatMap((row) => splitMask(row.event_mask).extras),
                ),
              ].map((code) => (
                <tr key={code} className="border-b border-border">
                  <th
                    scope="row"
                    className="px-2 py-2 text-start font-mono font-normal"
                  >
                    {code}
                  </th>
                  {rows.map((row) => (
                    <td key={row.id} className="px-2 py-2">
                      <Checkbox
                        checked={splitMask(row.event_mask).extras.includes(
                          code,
                        )}
                        disabled
                        aria-label={t("notifications.eventFor", {
                          event: code,
                          name: row.name,
                        })}
                      />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {dialog !== null && (
        <ChannelDialog
          state={dialog}
          onClose={(changed) => {
            // A saved edit can change the endpoint or secret, so a stale
            // test verdict for the old configuration must not survive.
            if (changed && dialog.mode === "edit") {
              const id = dialog.channel.id;
              setResults((current) => {
                const next = { ...current };
                delete next[id];
                return next;
              });
            }
            setDialog(null);
            if (changed) void channels.refetch();
          }}
        />
      )}

      {deleteTarget !== null && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setDeleteTarget(null);
          }}
        >
          <DialogContent
            className="sm:max-w-md"
            aria-label={t("notifications.deleteTitle")}
          >
            <DialogHeader>
              <DialogTitle>{t("notifications.deleteTitle")}</DialogTitle>
              <DialogDescription>
                {t("notifications.deleteBody", { name: deleteTarget.name })}
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setDeleteTarget(null)}>
                {t("notifications.deleteCancel")}
              </Button>
              <Button onClick={() => void deleteChannel()}>
                {t("notifications.deleteConfirm")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}

function FragmentRow({
  row,
  result,
  toggling,
  sending,
  onToggle,
  onTest,
  onEdit,
  onDelete,
}: {
  row: ChannelRow;
  result: TestReply | undefined;
  toggling: boolean;
  sending: boolean;
  onToggle: (enabled: boolean) => void;
  onTest: () => void;
  onEdit: () => void;
  onDelete: () => void;
}): JSX.Element {
  const { t } = useTranslation("settings");
  return (
    <>
      <tr
        className={`border-b border-border align-top ${
          toggling ? "animate-pulse" : ""
        }`}
        aria-busy={toggling}
      >
        <td className="px-2 py-2">
          <span className="font-medium">{row.name}</span>
          {row.last_error !== null && (
            <p className="mt-1 text-xs" style={{ color: "var(--warn)" }}>
              {row.last_error}
            </p>
          )}
        </td>
        <td className="px-2 py-2">{t(`notifications.kinds.${row.kind}`)}</td>
        <td className="px-2 py-2">
          <Checkbox
            checked={row.enabled}
            disabled={toggling}
            aria-label={t("notifications.enableFor", { name: row.name })}
            onCheckedChange={(checked) => onToggle(checked === true)}
          />
        </td>
        <td className="px-2 py-2">
          {row.last_send_at === null ? (
            t("notifications.neverSent")
          ) : (
            <span title={formatAbsolute(row.last_send_at)}>
              {formatWhen(row.last_send_at)}
            </span>
          )}
        </td>
        <td className="px-2 py-2">
          <div className="flex items-center gap-1">
            <Button
              variant="outline"
              size="sm"
              disabled={sending}
              aria-label={t("notifications.sendTestFor", {
                name: row.name,
              })}
              onClick={onTest}
            >
              {sending
                ? t("notifications.sendingTest")
                : t("notifications.sendTest")}
            </Button>
            <Button variant="outline" size="sm" onClick={onEdit}>
              {t("notifications.edit")}
            </Button>
            <Button variant="outline" size="sm" onClick={onDelete}>
              {t("notifications.delete")}
            </Button>
          </div>
        </td>
      </tr>
      {result !== undefined && (
        <tr className="border-b border-border">
          <td colSpan={5} className="px-2 py-2">
            <RawReplyBlock result={result} />
          </td>
        </tr>
      )}
    </>
  );
}

/** The verbatim reply of doc 05 §14.1: status line, headers and the first
 *  8 KiB of body rendered as-is in monospace; a null response renders the
 *  transport error string in the same block. Nothing is parsed, reformatted
 *  or summarised. */
function RawReplyBlock({ result }: { result: TestReply }): JSX.Element {
  const { t } = useTranslation("settings");
  return (
    <div
      aria-live="polite"
      className="rounded-md border border-border bg-muted/50 px-3 py-2 font-mono text-xs"
    >
      <div className="flex flex-wrap gap-x-4 gap-y-0.5 text-muted-foreground">
        <span>
          {t("notifications.testOk")}:{" "}
          {result.ok
            ? t("notifications.testPassed")
            : t("notifications.testFailedVerdict")}
        </span>
        <span>
          {t("notifications.testElapsed")}: {result.elapsed_ms}
        </span>
      </div>
      <p className="mt-1 break-all">
        {result.request.method} {result.request.url}
      </p>
      {result.response === null || result.response === undefined ? (
        <pre className="mt-1 whitespace-pre-wrap">{result.error}</pre>
      ) : (
        <>
          <pre className="mt-1 whitespace-pre-wrap">
            {result.response.status_line}
          </pre>
          <dl className="mt-1">
            <dt className="text-muted-foreground">
              {t("notifications.testHeaders")}
            </dt>
            <dd>
              <pre className="whitespace-pre-wrap">
                {Object.entries(result.response.headers)
                  .map(([name, value]) => `${name}: ${value}`)
                  .join("\n")}
              </pre>
            </dd>
          </dl>
          <pre
            aria-label={t("notifications.testBody")}
            className="mt-1 max-h-48 overflow-auto whitespace-pre-wrap"
          >
            {result.response.body}
          </pre>
        </>
      )}
    </div>
  );
}
