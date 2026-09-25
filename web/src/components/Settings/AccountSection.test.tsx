import type { JSX } from "react";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HttpResponse, http } from "msw";
import { setupServer } from "msw/node";
import { toast } from "sonner";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  expect,
  test,
  vi,
} from "vitest";
import { initI18n } from "../../i18n";
import { AccountSection, type TokenRow } from "./AccountSection";
import { NotificationsSection, type ChannelRow } from "./NotificationsSection";

const server = setupServer();
let qc: QueryClient;

const ACCOUNT = {
  id: "usr_01",
  username: "operator",
  enabled: true,
  locale: "en",
  last_login_at: "2026-09-20T10:00:00Z",
  created_at: "2026-01-01T00:00:00Z",
};

const channel = (over: Partial<ChannelRow>): ChannelRow => ({
  id: "ntf_x",
  kind: "ntfy",
  name: "x",
  enabled: true,
  config: { server_url: "https://ntfy.example", topic: "dl" },
  secret_set: false,
  event_mask: ["*"],
  last_send_at: null,
  last_error: null,
  ...over,
});

const token = (over: Partial<TokenRow>): TokenRow => ({
  id: "tok_x",
  name: "x",
  prefix: "dlt_ab12",
  last_used_at: null,
  expires_at: null,
  created_at: "2026-09-01T00:00:00Z",
  ...over,
});

let tokens: TokenRow[];
let channels: ChannelRow[];
let accountPatches: Record<string, unknown>[];
let channelPatches: { id: string; body: Record<string, unknown> }[];
let accountStatus: number;
let accountProblem: Record<string, unknown> | null;

function mount(element: JSX.Element) {
  return render(
    <QueryClientProvider client={qc}>{element}</QueryClientProvider>,
  );
}

beforeAll(() => {
  initI18n();
  server.listen({ onUnhandledRequest: "error" });
});

beforeEach(() => {
  qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  tokens = [
    token({ id: "tok_1", name: "cli", last_used_at: "2026-09-21T09:00:00Z" }),
  ];
  channels = [channel({ id: "ntf_alerts", name: "Alerts", event_mask: ["*"] })];
  accountPatches = [];
  channelPatches = [];
  accountStatus = 200;
  accountProblem = null;
  server.use(
    http.get("*/api/v1/account", () => HttpResponse.json(ACCOUNT)),
    http.patch("*/api/v1/account", async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      accountPatches.push(body);
      if (accountProblem !== null)
        return HttpResponse.json(accountProblem, { status: accountStatus });
      return HttpResponse.json({
        ...ACCOUNT,
        username: (body.username as string | undefined) ?? ACCOUNT.username,
        locale: (body.locale as string | undefined) ?? ACCOUNT.locale,
      });
    }),
    http.get("*/api/v1/api-tokens", () =>
      HttpResponse.json({
        items: tokens,
        next_cursor: null,
        total: tokens.length,
      }),
    ),
    http.get("*/api/v1/notifications", () => HttpResponse.json({ channels })),
    http.patch("*/api/v1/notifications/:id", async ({ params, request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      channelPatches.push({ id: String(params.id), body });
      const base = channels.find((row) => row.id === params.id);
      if (base === undefined)
        return HttpResponse.json(
          { title: "Not Found", status: 404 },
          { status: 404 },
        );
      const updated = { ...base, ...body };
      // The later refetch must serve the patch the way the real store
      // would, or the matrix reverts between onMutate and onSettled.
      channels = channels.map((row) => (row.id === params.id ? updated : row));
      return HttpResponse.json(updated);
    }),
  );
});

afterEach(() => {
  cleanup();
  qc.clear();
  server.resetHandlers();
  vi.restoreAllMocks();
});

afterAll(() => server.close());

// TestTokenRevealedOnceOnly pins doc 05 §12's create-once rule: the 201
// body's token renders inside the reveal dialog, closing the dialog clears
// the state so it appears in no later render, and the value is never in a
// token row (the list carries only prefix) nor handed to storage.
test("TestTokenRevealedOnceOnly", async () => {
  const TOKEN = "dlt_0123456789abcdef0123456789abcdef";
  const setItem = vi.spyOn(Storage.prototype, "setItem");
  server.use(
    http.post("*/api/v1/api-tokens", async ({ request }) => {
      const body = (await request.json()) as { name: string };
      const created = token({
        id: "tok_new",
        name: body.name,
        prefix: TOKEN.slice(0, 8),
      });
      tokens = [...tokens, created];
      return HttpResponse.json({ ...created, token: TOKEN }, { status: 201 });
    }),
  );
  mount(<AccountSection />);
  await screen.findByLabelText("Username");

  fireEvent.change(screen.getByLabelText("Token name"), {
    target: { value: "backup script" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create token" }));

  // The reveal dialog is the only place the value ever renders.
  const dialog = await screen.findByRole("dialog");
  expect(within(dialog).getByText(TOKEN)).toBeTruthy();
  expect(within(dialog).getByText(/only time the token is shown/)).toBeTruthy();

  // Closing clears the holding state: the value is gone from the document
  // and cannot come back — no row and no later render contains it.
  fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await screen.findByText("backup script", { selector: "td.font-medium" });
  expect(screen.queryByText(TOKEN)).toBeNull();

  // The row shows the prefix, never the value.
  const row = screen
    .getByText("backup script", { selector: "td.font-medium" })
    .closest("tr") as HTMLElement;
  expect(row.textContent ?? "").toContain("dlt_0123");
  expect(row.textContent ?? "").not.toContain(TOKEN);

  // No storage write ever carried the token.
  for (const call of setItem.mock.calls)
    expect(String(call[1])).not.toContain(TOKEN);
});

// TestPasswordChangeSendsCurrentPassword pins the wire shape of doc 05 §12:
// the PATCH carries password and current_password in the same body.
test("TestPasswordChangeSendsCurrentPassword", async () => {
  mount(<AccountSection />);
  await screen.findByLabelText("Username");

  fireEvent.change(screen.getByLabelText("Current password"), {
    target: { value: "the old password" },
  });
  fireEvent.change(screen.getByLabelText("New password"), {
    target: { value: "a brand new passphrase" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Change password" }));

  await waitFor(() => expect(accountPatches.length).toBe(1));
  expect(accountPatches[0]).toEqual({
    password: "a brand new passphrase",
    current_password: "the old password",
  });
  await screen.findByText(/Every other session was signed out/);
});

// TestWrongCurrentPasswordRendersInline pins the 403 mapping: the message
// lands on the current_password input as a field-level alert, not a toast.
test("TestWrongCurrentPasswordRendersInline", async () => {
  const toastError = vi.spyOn(toast, "error");
  accountStatus = 403;
  accountProblem = {
    type: "/problems/forbidden",
    title: "Forbidden",
    detail: "the current password does not match",
    status: 403,
  };
  mount(<AccountSection />);
  await screen.findByLabelText("Username");

  const current = screen.getByLabelText("Current password") as HTMLInputElement;
  fireEvent.change(current, { target: { value: "wrong password" } });
  fireEvent.change(screen.getByLabelText("New password"), {
    target: { value: "a brand new passphrase" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Change password" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe("The current password does not match.");
  expect(current.getAttribute("aria-invalid")).toBe("true");
  expect(current.getAttribute("aria-describedby")).toBe(alert.id);
  expect(toastError).not.toHaveBeenCalled();
});

// TestMatrixRoundTripsUnknownCode pins the preservation rule: a stored
// event_mask entry outside NOTIFIABLE_EVENTS renders as an extra read-only
// row and survives any matrix save unchanged.
test("TestMatrixRoundTripsUnknownCode", async () => {
  channels = [
    channel({
      id: "ntf_legacy",
      name: "Legacy",
      event_mask: ["task.completed", "legacy.custom_code"],
    }),
  ];
  mount(<NotificationsSection />);
  await screen.findByText("Legacy", { selector: "span.font-medium" });

  // The unknown code is its own read-only row: rendered, checked, disabled.
  const extraRow = screen
    .getByText("legacy.custom_code")
    .closest("tr") as HTMLElement;
  const extraBox = within(extraRow).getByRole("checkbox");
  expect(extraBox.getAttribute("aria-checked")).toBe("true");
  expect(extraBox).toHaveProperty("disabled", true);

  // Toggling a known event PATCHes a mask that still carries the extra code.
  const completedRow = screen
    .getByText("Task completed")
    .closest("tr") as HTMLElement;
  const errorRow = screen.getByText("Task error").closest("tr") as HTMLElement;
  expect(
    within(completedRow).getByRole("checkbox").getAttribute("aria-checked"),
  ).toBe("true");
  fireEvent.click(within(errorRow).getByRole("checkbox"));
  await waitFor(() => expect(channelPatches.length).toBe(1));
  expect(channelPatches[0].id).toBe("ntf_legacy");
  expect(channelPatches[0].body).toEqual({
    event_mask: ["task.completed", "task.error", "legacy.custom_code"],
  });
});

// TestAllEventsRowSendsStar pins the wildcard row: checking it replaces the
// mask with ["*"] and disables the per-event boxes; unchecking restores the
// codes that were checked.
test("TestAllEventsRowSendsStar", async () => {
  channels = [
    channel({
      id: "ntf_partial",
      name: "Partial",
      event_mask: ["task.completed", "task.error"],
    }),
  ];
  mount(<NotificationsSection />);
  await screen.findByText("Partial", { selector: "span.font-medium" });

  fireEvent.click(
    screen.getByRole("checkbox", { name: "All events for Partial" }),
  );
  await waitFor(() => expect(channelPatches.length).toBe(1));
  expect(channelPatches[0]).toEqual({
    id: "ntf_partial",
    body: { event_mask: ["*"] },
  });

  // While the mask is ["*"] every per-event box renders checked+disabled;
  // task.created was unchecked before, so its flip proves the PATCH applied.
  const createdRow = screen
    .getByText("Task created")
    .closest("tr") as HTMLElement;
  const box = within(createdRow).getByRole("checkbox");
  await waitFor(() => expect(box.getAttribute("aria-checked")).toBe("true"));
  expect(box).toHaveProperty("disabled", true);

  // Unchecking restores the codes that were checked before.
  fireEvent.click(
    screen.getByRole("checkbox", { name: "All events for Partial" }),
  );
  await waitFor(() => expect(channelPatches.length).toBe(2));
  expect(channelPatches[1].body).toEqual({
    event_mask: ["task.completed", "task.error"],
  });
});

// TestStarWithUnknownCode pins which rule wins when both apply: checking
// the star replaces the whole mask with ["*"] (extras included), and
// unchecking restores the remembered mask — unknown code and all.
test("TestStarWithUnknownCode", async () => {
  channels = [
    channel({
      id: "ntf_mixed",
      name: "Mixed",
      event_mask: ["task.completed", "legacy.custom_code"],
    }),
  ];
  mount(<NotificationsSection />);
  await screen.findByText("Mixed", { selector: "span.font-medium" });

  fireEvent.click(
    screen.getByRole("checkbox", { name: "All events for Mixed" }),
  );
  await waitFor(() => expect(channelPatches.length).toBe(1));
  expect(channelPatches[0].body).toEqual({ event_mask: ["*"] });

  // The box is disabled while the first PATCH is in flight; wait it out
  // rather than racing a click the component would drop.
  const star = screen.getByRole("checkbox", { name: "All events for Mixed" });
  await waitFor(() => expect(star).toHaveProperty("disabled", false));
  fireEvent.click(star);
  await waitFor(() => expect(channelPatches.length).toBe(2));
  expect(channelPatches[1].body).toEqual({
    event_mask: ["task.completed", "legacy.custom_code"],
  });
});

// TestWeakNewPasswordMapsToPasswordField is the 422 sibling of the 403
// pin: a validation problem lands on the new-password input as a
// field-level alert, not a toast.
test("TestWeakNewPasswordMapsToPasswordField", async () => {
  const toastError = vi.spyOn(toast, "error");
  accountStatus = 422;
  accountProblem = {
    type: "/problems/validation-failed",
    title: "Validation failed",
    detail: "password is shorter than 12 characters",
    status: 422,
  };
  mount(<AccountSection />);
  await screen.findByLabelText("Username");

  // Twelve or more characters so the client-side floor lets the PATCH
  // through and the server's 422 is what answers.
  const next = screen.getByLabelText("New password") as HTMLInputElement;
  fireEvent.change(screen.getByLabelText("Current password"), {
    target: { value: "the old password" },
  });
  fireEvent.change(next, { target: { value: "a brand new passphrase" } });
  fireEvent.click(screen.getByRole("button", { name: "Change password" }));

  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toBe("password is shorter than 12 characters");
  expect(next.getAttribute("aria-invalid")).toBe("true");
  expect(next.getAttribute("aria-describedby")).toBe(alert.id);
  expect(toastError).not.toHaveBeenCalled();
});

// TestTokenRevokeRemovesRow pins the revoke flow: the confirm dialog's
// DELETE fires once and the refetched list drops the row — revoked rows
// are the server's audit trail and never list.
test("TestTokenRevokeRemovesRow", async () => {
  const deleted: string[] = [];
  server.use(
    http.delete("*/api/v1/api-tokens/:id", ({ params }) => {
      deleted.push(String(params.id));
      tokens = tokens.filter((row) => row.id !== params.id);
      return new HttpResponse(null, { status: 204 });
    }),
  );
  mount(<AccountSection />);
  const cell = await screen.findByText("cli", { selector: "td.font-medium" });
  fireEvent.click(
    within(cell.closest("tr") as HTMLElement).getByRole("button", {
      name: "Revoke",
    }),
  );
  const dialog = await screen.findByRole("dialog");
  fireEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));
  await waitFor(() => expect(deleted).toEqual(["tok_1"]));
  await waitFor(() =>
    expect(
      screen.queryByText("cli", { selector: "td.font-medium" }),
    ).toBeNull(),
  );
});

// TestChannelSecretFieldIsWriteOnly pins doc 05 §14's secret rule: a
// channel with secret_set renders the marker and an empty input, never a
// stored value or a redaction stand-in.
test("TestChannelSecretFieldIsWriteOnly", async () => {
  channels = [channel({ id: "ntf_secret", name: "Ops", secret_set: true })];
  mount(<NotificationsSection />);
  await screen.findByText("Ops", { selector: "span.font-medium" });

  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  const dialog = await screen.findByRole("dialog");
  const secretInput = within(dialog).getByLabelText(
    /^Secret/,
  ) as HTMLInputElement;
  expect(secretInput.value).toBe("");
  expect(within(dialog).getByText("Stored")).toBeTruthy();
  expect(dialog.textContent ?? "").not.toContain("__redacted__");
});

// TestSendTestRendersRawReply pins doc 05 §14.1: status_line and body
// render verbatim, and a 200 ok:false reply is data, not a toast.
test("TestSendTestRendersRawReply", async () => {
  const toastError = vi.spyOn(toast, "error");
  const BODY = '{"code":40301,"http":403,"error":"forbidden"}';
  server.use(
    http.post("*/api/v1/notifications/ntf_alerts/test", () =>
      HttpResponse.json({
        ok: false,
        elapsed_ms: 214,
        request: { method: "POST", url: "https://ntfy.sh/dl-tool-alice" },
        response: {
          status_line: "HTTP/1.1 403 Forbidden",
          status: 403,
          headers: { "content-type": "application/json" },
          body: BODY,
        },
        error: null,
      }),
    ),
  );
  mount(<NotificationsSection />);
  await screen.findByText("Alerts", { selector: "span.font-medium" });

  fireEvent.click(
    screen.getByRole("button", { name: "Send a test event to Alerts" }),
  );

  await screen.findByText("HTTP/1.1 403 Forbidden");
  expect(screen.getByText(BODY)).toBeTruthy();
  expect(screen.getByText("POST https://ntfy.sh/dl-tool-alice")).toBeTruthy();
  expect(screen.getByText("content-type: application/json")).toBeTruthy();
  expect(toastError).not.toHaveBeenCalled();

  // A transport failure renders the error string in the same block.
  server.use(
    http.post("*/api/v1/notifications/ntf_alerts/test", () =>
      HttpResponse.json({
        ok: false,
        elapsed_ms: 3,
        request: { method: "POST", url: "https://ntfy.sh/dl-tool-alice" },
        response: null,
        error: "dial tcp: connection refused",
      }),
    ),
  );
  fireEvent.click(
    screen.getByRole("button", { name: "Send a test event to Alerts" }),
  );
  await screen.findByText("dial tcp: connection refused");
  // The second reply replaces the first: nothing of it stays on screen.
  expect(screen.queryByText("HTTP/1.1 403 Forbidden")).toBeNull();
  expect(screen.queryByText(BODY)).toBeNull();
  expect(toastError).not.toHaveBeenCalled();
});
