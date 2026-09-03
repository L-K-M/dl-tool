-- +goose Up
CREATE TABLE users (
  id TEXT PRIMARY KEY,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,          -- argon2id PHC string: $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  locale TEXT NOT NULL DEFAULT 'en', last_login_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,      -- SHA-256 hex of the cookie value; the value itself is never stored
  csrf_token TEXT NOT NULL, expires_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  ip TEXT, user_agent TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE api_tokens (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,      -- SHA-256 hex; the bearer token is shown once, at creation
  prefix TEXT NOT NULL,                 -- first 8 characters, for display only
  last_used_at INTEGER, expires_at INTEGER, revoked_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE settings (
  id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, value_json TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE engines (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL UNIQUE CHECK (kind IN ('aria2','qbittorrent','ytdlp')),
  name TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  url TEXT,                             -- aria2 JSON-RPC URL / qBittorrent base URL; NULL for ytdlp
  username TEXT,
  secret_enc TEXT,                      -- encrypted at rest; never returned by an API, never logged
  binary_path TEXT,                     -- ytdlp only
  version TEXT,                         -- written by the boot capability probe: the resolved engine version,
                                        -- and for 'ytdlp' the yt-dlp version and the JS runtime version
  last_seen_at INTEGER, last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE categories (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, save_path TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE tags (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE notification_channels (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('webhook','ntfy','gotify','apprise')),
  name TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  config_json TEXT NOT NULL,            -- non-secret channel configuration; shape per kind, see §4.8
  secret_enc TEXT,                      -- encrypted at rest; never returned by an API, never logged
  event_mask TEXT NOT NULL DEFAULT '["*"]',  -- JSON array of task_events.code values; ["*"] = every code
  last_send_at INTEGER,                 -- unix ms of the last delivery attempt; written by T077
  last_error TEXT,                      -- failure text of the last attempt, NULL after a success
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_notification_channels_enabled ON notification_channels(enabled);

INSERT INTO settings (id, key, value_json, created_at, updated_at) VALUES
  ('set_00000000000000000000000001', 'max_active_total', '5', 0, 0),
  ('set_00000000000000000000000002', 'max_active_per_engine', '3', 0, 0),
  ('set_00000000000000000000000003', 'min_free_space', '{}', 0, 0);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  engine TEXT NOT NULL CHECK (engine IN ('aria2','qbittorrent','ytdlp')),
  engine_ref TEXT,                      -- aria2 GID | qBittorrent infohash | yt-dlp job id; NULL until accepted
  source_kind TEXT NOT NULL CHECK (source_kind IN ('http','ftp','sftp','magnet','torrent','metalink','media')),
  source_uri TEXT,                       -- server-only engine/recovery source; may contain provider credentials
  source_display_uri TEXT,               -- API-safe; search-result:<res_id> for a grabbed result
  name TEXT NOT NULL,
  infohash_v1 TEXT,                     -- exactly 40 lowercase hex chars, or NULL
  infohash_v2 TEXT,                     -- exactly 64 lowercase hex chars, or NULL
  state TEXT NOT NULL CHECK (state IN ('queued','downloading','seeding','paused','checking',
                                       'extracting','moving','completed','error','removed')),
  error_code TEXT CHECK (error_code IS NULL OR error_code IN (
    'broken_link','destination_not_exist','destination_denied','disk_full','quota_reached','timeout',
    'exceed_max_file_system_size','exceed_max_destination_size','exceed_max_temp_size',
    'encrypted_name_too_long','name_too_long','torrent_duplicate','file_not_exist',
    'required_premium_account','not_supported_type','try_it_later','task_encryption','missing_python',
    'private_video','ftp_encryption_not_supported_type','extract_failed','extract_failed_wrong_password',
    'extract_failed_invalid_archive','extract_failed_quota_reached','extract_failed_disk_full','unknown',
    'ssrf_blocked','path_rejected','engine_unavailable','unsupported_scheme',
    'concurrency_limit','js_runtime_missing')),
  error_message TEXT, destination TEXT NOT NULL,
  requested_destination TEXT,             -- what the client asked for, when the server resolved a
                                         -- different effective destination (FR-044); NULL when identical
  content_path TEXT,                    -- absolute path to the finished file or directory
  category_id TEXT REFERENCES categories(id) ON DELETE SET NULL,
  total_bytes INTEGER,                  -- NULL while metadata is unknown
  completed_bytes INTEGER NOT NULL DEFAULT 0, uploaded_bytes INTEGER NOT NULL DEFAULT 0,
  download_rate INTEGER NOT NULL DEFAULT 0, upload_rate INTEGER NOT NULL DEFAULT 0,
  eta_seconds INTEGER, ratio REAL NOT NULL DEFAULT 0,
  total_peers INTEGER NOT NULL DEFAULT 0, connected_seeders INTEGER NOT NULL DEFAULT 0,
  connected_leechers INTEGER NOT NULL DEFAULT 0,
  dl_limit INTEGER NOT NULL DEFAULT 0, ul_limit INTEGER NOT NULL DEFAULT 0,
  ratio_limit REAL, seeding_time_limit INTEGER,          -- seconds
  sequential INTEGER NOT NULL DEFAULT 0 CHECK (sequential IN (0,1)),
  queue_position INTEGER,
  unzip_progress INTEGER,               -- 0-100, only while state = 'extracting'
  extract_password TEXT,                -- secret: never returned by an API, never logged; encrypted
                                         -- at rest with DLTOOL_SECRET_KEY (11-config-reference.md §6)
  added_at INTEGER NOT NULL, started_at INTEGER, completed_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_tasks_engine_ref ON tasks(engine, engine_ref) WHERE engine_ref IS NOT NULL AND state <> 'removed';
CREATE UNIQUE INDEX idx_tasks_infohash_v1 ON tasks(infohash_v1) WHERE infohash_v1 IS NOT NULL AND state <> 'removed';
CREATE UNIQUE INDEX idx_tasks_infohash_v2 ON tasks(infohash_v2) WHERE infohash_v2 IS NOT NULL AND state <> 'removed';
CREATE INDEX idx_tasks_state ON tasks(state, added_at DESC);
CREATE INDEX idx_tasks_category ON tasks(category_id);
CREATE INDEX idx_tasks_updated ON tasks(updated_at);

CREATE TABLE task_tags (
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (task_id, tag_id));
CREATE INDEX idx_task_tags_tag ON task_tags(tag_id);

CREATE TABLE task_files (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  file_index INTEGER NOT NULL,          -- engine-side index, 0-based
  path TEXT NOT NULL,                   -- relative to tasks.destination
  size_bytes INTEGER NOT NULL DEFAULT 0, completed_bytes INTEGER NOT NULL DEFAULT 0,
  selected INTEGER NOT NULL DEFAULT 1 CHECK (selected IN (0,1)),
  priority INTEGER CHECK (priority IS NULL OR priority IN (0,1,6,7)),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_task_files_idx ON task_files(task_id, file_index);

CREATE TABLE task_trackers (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  url TEXT NOT NULL,
  status TEXT,                          -- engine-reported string, stored verbatim
  update_timer_seconds INTEGER, seeds INTEGER, peers INTEGER, message TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_task_trackers_url ON task_trackers(task_id, url);

CREATE TABLE task_events (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  at INTEGER NOT NULL,                  -- unix ms
  level TEXT NOT NULL CHECK (level IN ('info','warn','error')),
  code TEXT NOT NULL,                   -- stable i18n key: 'engine.accepted', 'postprocess.unrar.failed', ...
  message TEXT NOT NULL, detail_json TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_task_events_task ON task_events(task_id, at DESC);
CREATE INDEX idx_task_events_at ON task_events(at);

CREATE TABLE indexers (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('torznab','newznab','dlsearch')),
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),   -- imported indexers start disabled
  url TEXT,                             -- Torznab/Newznab base URL
  api_key_enc TEXT,                     -- encrypted at rest; never returned by an API
  definition_id TEXT,                   -- dlsearch engine id, e.g. 'internet-archive'
  definition_source TEXT CHECK (definition_source IS NULL OR definition_source IN ('bundled','user','imported')),
  provenance TEXT,                      -- shown in the UI, e.g. 'imported from jackett.dlm'
  legal_tier TEXT NOT NULL DEFAULT 'user-supplied' CHECK (legal_tier IN ('legitimate','user-supplied')),
  priority INTEGER NOT NULL DEFAULT 50,
  allow_private_network INTEGER NOT NULL DEFAULT 0 CHECK (allow_private_network IN (0,1)),
  seeders_unknown INTEGER NOT NULL DEFAULT 0 CHECK (seeders_unknown IN (0,1)),
  settings_json TEXT, categories_json TEXT, last_test_at INTEGER, last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_indexers_definition ON indexers(definition_id) WHERE definition_id IS NOT NULL;

CREATE TABLE search_jobs (
  id TEXT PRIMARY KEY,
  query TEXT NOT NULL,
  indexer_ids_json TEXT NOT NULL,       -- JSON array of indexers.id
  categories_json TEXT,                 -- JSON array of newznab category ids
  finished INTEGER NOT NULL DEFAULT 0 CHECK (finished IN (0,1)),
  total INTEGER NOT NULL DEFAULT 0, last_error TEXT,
  started_at INTEGER NOT NULL, finished_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_search_jobs_created ON search_jobs(created_at);

CREATE TABLE search_results (
  id TEXT PRIMARY KEY,
  search_job_id TEXT NOT NULL REFERENCES search_jobs(id) ON DELETE CASCADE,
  indexer_id TEXT NOT NULL REFERENCES indexers(id) ON DELETE CASCADE,
  -- Provider URLs and magnets are server-only acquisition data; never serialize them.
  title TEXT NOT NULL, download_url TEXT, magnet_uri TEXT, info_hash TEXT, size_bytes INTEGER,
  seeders INTEGER,                      -- NULL when unknown; never -1, never a fabricated 1
  leechers INTEGER,                     -- leechers = peers - seeders when only peers is given
  grabs INTEGER, published_at INTEGER, details_url TEXT,
  category_ids_json TEXT, category_desc TEXT,
  download_volume_factor REAL NOT NULL DEFAULT 1.0, upload_volume_factor REAL NOT NULL DEFAULT 1.0,
  minimum_ratio REAL, minimum_seed_time_seconds INTEGER,
  imdb_id TEXT, tmdb_id TEXT, tvdb_id TEXT, year INTEGER,
  genre TEXT, language TEXT, publisher TEXT, author TEXT, album TEXT, artist TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_search_results_job ON search_results(search_job_id);

CREATE TABLE feeds (
  id TEXT PRIMARY KEY, url TEXT NOT NULL UNIQUE, title TEXT,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  refresh_interval_s INTEGER NOT NULL DEFAULT 0,   -- 0 = use the global RSS interval setting
  item_cap INTEGER NOT NULL DEFAULT 50,            -- retained feed_items per feed
  priority INTEGER NOT NULL DEFAULT 0,             -- per-run tie-break in 08 §5 step 13; lower wins
  etag TEXT,
  last_modified TEXT,                   -- verbatim HTTP-date string, replayed as If-Modified-Since
  ttl_minutes INTEGER, last_fetch_at INTEGER, last_success_at INTEGER,
  next_fetch_at INTEGER NOT NULL, escalation_level INTEGER NOT NULL DEFAULT 0,
  disabled_till INTEGER, last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_feeds_next_fetch ON feeds(next_fetch_at) WHERE enabled = 1;

CREATE TABLE feed_items (
  id TEXT PRIMARY KEY,
  feed_id TEXT NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
  guid TEXT,                            -- raw <guid>/<id>, NULL when absent
  identity TEXT NOT NULL,               -- resolved dedup identity
  title TEXT NOT NULL,
  title_norm TEXT NOT NULL,             -- lowercased, [._] -> space, whitespace collapsed
  link TEXT,
  download_url TEXT,                    -- .torrent URL or magnet:
  info_hash TEXT,                       -- lowercase hex: 40 chars (v1) or 64 chars (v2); NULL when unknown
  size_bytes INTEGER, published_at INTEGER, first_seen_at INTEGER NOT NULL,
  read INTEGER NOT NULL DEFAULT 0 CHECK (read IN (0,1)),  -- 1 once the user marks the item read
  raw_json TEXT,                        -- full parsed item; feeds the dry-run panel
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_feed_items_identity ON feed_items(feed_id, identity);
CREATE INDEX idx_feed_items_read ON feed_items(feed_id, read, published_at DESC);
CREATE INDEX idx_feed_items_hash ON feed_items(info_hash);
CREATE INDEX idx_feed_items_norm ON feed_items(title_norm);
CREATE INDEX idx_feed_items_pub ON feed_items(feed_id, published_at DESC);

CREATE TABLE rules (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  priority INTEGER NOT NULL DEFAULT 0,  -- lower is evaluated first; ties broken by name
  definition_json TEXT NOT NULL,        -- the rule document; schema in 08-rss-automation.md
  last_match_at INTEGER,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

CREATE TABLE rule_matches (
  id TEXT PRIMARY KEY,
  rule_id TEXT REFERENCES rules(id) ON DELETE SET NULL,
  feed_item_id TEXT REFERENCES feed_items(id) ON DELETE SET NULL,
  task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
  info_hash TEXT,                       -- back-filled after the .torrent is fetched; 40 or 64 lowercase hex
  content_key TEXT,                     -- e.g. 'tv:the-show:s01e05'
  title TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('queued','sent','failed','rejected','fallback')),
  reason TEXT,                          -- rejection reason code; vocabulary in 08-rss-automation.md
  score INTEGER NOT NULL DEFAULT 0, matched_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_rule_matches_hash ON rule_matches(info_hash) WHERE info_hash IS NOT NULL;
CREATE INDEX idx_rule_matches_key ON rule_matches(content_key) WHERE content_key IS NOT NULL;
CREATE INDEX idx_rule_matches_rule ON rule_matches(rule_id, matched_at DESC);

CREATE TABLE rule_seen_episodes (
  rule_id TEXT NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
  episode_key TEXT NOT NULL,            -- '1x5', '1x5-REPACK', '2017.01.01'
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (rule_id, episode_key));

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,                   -- 'postprocess' | 'extract' | 'move' | 'webhook' | 'rss_poll' | ...
  task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE,
  payload_json TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','done','failed')),
  attempts INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 5,
  run_after INTEGER NOT NULL,           -- unix ms
  locked_at INTEGER,                    -- unix ms, NULL when unclaimed
  last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE INDEX idx_jobs_claim ON jobs(state, run_after);

CREATE TABLE bandwidth_schedule (
  id TEXT PRIMARY KEY,
  day INTEGER NOT NULL CHECK (day BETWEEN 0 AND 6),      -- 0 = Monday
  hour INTEGER NOT NULL CHECK (hour BETWEEN 0 AND 23),
  mode TEXT NOT NULL CHECK (mode IN ('no_download','default','alternative')),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_bandwidth_schedule_cell ON bandwidth_schedule(day, hour);

CREATE TABLE ui_prefs (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key TEXT NOT NULL, value_json TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX idx_ui_prefs_key ON ui_prefs(user_id, key);

CREATE TABLE watch_folders (
  id TEXT PRIMARY KEY, path TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  destination TEXT NOT NULL,
  category_id TEXT REFERENCES categories(id) ON DELETE SET NULL,
  delete_after_load INTEGER NOT NULL DEFAULT 0 CHECK (delete_after_load IN (0,1)),
  poll_interval_s INTEGER NOT NULL DEFAULT 10,   -- polling fallback when inotify registration fails
  last_scan_at INTEGER, last_error TEXT,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);

WITH RECURSIVE
  days(day) AS (VALUES (0) UNION ALL SELECT day + 1 FROM days WHERE day < 6),
  hours(hour) AS (VALUES (0) UNION ALL SELECT hour + 1 FROM hours WHERE hour < 23)
INSERT INTO bandwidth_schedule (id, day, hour, mode, created_at, updated_at)
-- Prefix + 23 zeros + 3 digits is one 30-character prefixed ULID.
SELECT printf('bws_00000000000000000000000%03d', day * 24 + hour),
       day, hour, 'default', 0, 0
FROM days CROSS JOIN hours;

-- +goose Down
DROP TABLE watch_folders;
DROP TABLE ui_prefs;
DROP TABLE bandwidth_schedule;
DROP TABLE jobs;
DROP TABLE rule_seen_episodes;
DROP TABLE rule_matches;
DROP TABLE rules;
DROP TABLE feed_items;
DROP TABLE feeds;
DROP TABLE search_results;
DROP TABLE search_jobs;
DROP TABLE indexers;
DROP TABLE task_events;
DROP TABLE task_trackers;
DROP TABLE task_files;
DROP TABLE task_tags;
DROP TABLE tasks;
DROP TABLE notification_channels;
DROP TABLE tags;
DROP TABLE categories;
DROP TABLE engines;
DROP TABLE settings;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE users;
