# T072 — Build the RSS feeds and items screen

| Field | Value |
|---|---|
| **ID** | T072 |
| **Milestone** | M5 |
| **Status** | todo |
| **Depends on** | T014, T040, T044, T052, T065, T066 |
| **Blocks** | T073 |
| **Parallel-safe** | no — adds a route to T040's `web/src/App.tsx` |
| **Implements** | — (renders [FR-070](../02-requirements.md#fr-070-manage-feeds-and-refresh-on-demand) and [FR-072](../02-requirements.md#fr-072-extract-a-download-uri-from-each-item), covered by T065, T066 and T067) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`/rss/feeds` renders Download Station's RSS screen: a feed list with unread counts and error markers on the
left, the item table on the right, a preview card under it, and `Update` / `Update all`. Selecting items and
pressing *Download selected* creates tasks.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §8.1 Feeds and items](../09-web-ui-spec.md#81-feeds-and-items) — the
   wireframe, the feed states, the context menu, the Add-feed fields, the item columns and the preview card.
2. [`docs/05-api-contract.md` §10.1 Feeds](../05-api-contract.md#101-feeds) — every request and response
   this screen issues, including the refresh body.
3. [`docs/09-web-ui-spec.md` §2.4 Sidebar tree](../09-web-ui-spec.md#24-sidebar-tree) — the `RSS → Feeds`
   node and its unread count, and [§2.1 Routes](../09-web-ui-spec.md#21-routes) for `/rss/feeds`.
4. [`docs/09-web-ui-spec.md` §10.5 Empty states](../09-web-ui-spec.md#105-empty-states) and
   [§10.6 Toasts and optimistic updates](../09-web-ui-spec.md#106-toasts-and-optimistic-updates).
5. [`docs/14-conventions.md` §5 Frontend conventions](../14-conventions.md#5-frontend-conventions) — the
   generated client is the only transport, and every string goes through `t()`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Rss/FeedsScreen.tsx` | create | The two-pane screen, the toolbar and the Add-feed dialog. |
| `web/src/components/Rss/FeedsScreen.test.tsx` | create | Rendering, refresh, selection and error-state cases. |
| `web/src/locales/en/rss.json` | create | The `rss` namespace, shared with T073. |
| `web/src/App.tsx` | edit | Route `/rss/feeds` to the screen. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Rss/FeedsScreen.tsx
export type FeedState = 'ok' | 'loading' | 'error';

export interface FeedRow {
  id: string;                 // fed_…
  url: string;
  title: string | null;
  enabled: boolean;
  unread_count: number;
  last_error: string | null;
  state: FeedState;           // 'error' when last_error is set, 'loading' while a refresh is in flight
}

export interface ItemRow {
  id: string;                 // itm_…
  feed_id: string;
  title: string;
  link: string | null;
  download_url: string;
  info_hash: string | null;
  size_bytes: number | null;
  published_at: string | null; // RFC 3339
  read: boolean;
  matched_rules: { id: string; name: string }[];
}

export function FeedsScreen(): JSX.Element;

// Exported for the test and for T073's reuse of the feed list.
export function useFeeds(): { feeds: FeedRow[]; refresh: (id: string) => Promise<void>; refreshAll: () => Promise<void> };
```

Every call goes through the generated client of
[`docs/tasks/T014-typed-api-client.md`](T014-typed-api-client.md):
`GET /feeds`, `POST /feeds`, `PATCH /feeds/{id}`, `DELETE /feeds/{id}`, `POST /feeds/{id}/refresh`,
`GET /feeds/{id}/items?limit=&cursor=&unread=` and, for *Download selected*, `POST /tasks` with the items'
`download_url` values in `uris[]`.

Fixed labels, verbatim from doc 09 §8.1: the Add-feed dialog is **URL**, **Name**, **Folder**,
`☐ Automatically download all items`; the item actions are **Download selected**, **Mark read**,
**Mark all read**; the toolbar buttons are **Update** and **Update all**.

## Steps
1. Create `web/src/components/Rss/FeedsScreen.tsx` with a two-pane layout: the feed list left, the item
   table and the preview card right.
2. Render each feed as `●` OK, `◐` loading, `⚠` error, put `last_error` in the row's `title` attribute, and
   show the unread count as the doc 09 §2.4 pill.
3. Implement the context menu with Update · Rename… · Edit URL… · Mark all read · Copy feed URL · Remove,
   each backed by the endpoint above; `Remove` confirms first.
4. Implement the Add-feed dialog with the four fields above, posting `POST /feeds` and surfacing `409` as
   `This feed is already subscribed` and `422` as an inline field error.
5. Render the item table with a checkbox column, Title (unread bold with a filled dot, read muted), Feed,
   Age through `Intl.RelativeTimeFormat`, and one chip per entry of `matched_rules` whose tooltip names the
   rule.
6. Wire *Download selected* to `POST /tasks` with the selected `download_url` values, showing one toast per
   rejected URI and none on success beyond the created-count toast.
7. Render the preview card for the focused item — title, publication date, `Intl`-formatted size, category
   and the extracted download URI — with `Download`, `Mark read` and `Open link`.
8. Treat read state as server-derived: render `read` as it arrives and re-fetch after `Mark read`; do not
   keep a local read set.
9. Create `web/src/locales/en/rss.json` with every string this screen and T073's editor use, and add no
   bare literal to the TSX.
10. Edit `web/src/App.tsx` to route `/rss/feeds` to `FeedsScreen`.
11. Create `web/src/components/Rss/FeedsScreen.test.tsx` with `msw` handlers: feeds render with their
    counts; a feed carrying `last_error` renders `⚠` and the tooltip; `Update` posts the refresh and shows
    `items_added`; an item page loads and paginates; *Download selected* posts the exact `uris[]`; a
    `409` on Add-feed renders the duplicate message.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestFeedListRendersStatesAndCounts` and `TestRefreshShowsItemsAdded` pass.
- [ ] `TestDownloadSelectedPostsDownloadUrls` asserts the request body's `uris[]` exactly.
- [ ] `TestDuplicateFeedShowsConflictMessage` passes.
- [ ] The route `/rss/feeds` resolves and the sidebar `RSS → Feeds` node marks itself `aria-current`.
- [ ] No component in `web/src/components/Rss/` calls `fetch` directly.
- [ ] Every size, rate and date renders through `Intl`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo RSS_FEEDS_OK
```
Expected: Vitest reports every test file passing, including
`src/components/Rss/FeedsScreen.test.tsx` with the tests named above, and the final line of stdout is
exactly `RSS_FEEDS_OK`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT build the rule editor or the live preview; T073 owns `/rss/rules`.
- Do NOT implement feed folders as a server concept: doc 04 §3.5 has no folder table, so the `Folder` field
  groups the list client-side only.
- Do NOT parse a feed, a magnet or a `.torrent` in the browser; the server already extracted
  `download_url`.
- Do NOT subscribe this screen to the SSE stream; feeds refetch on action, and the sidebar count comes
  from the existing `sync` payload.
- Do NOT add an OPML import or a qBittorrent feed import; neither is in the product.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
