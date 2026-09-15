-- +goose Up
-- The persisted create-time file selection of docs/05-api-contract.md
-- section 5.2: one JSON document per task naming the indices to download
-- and the resolved per-file priority. The admission pass and the
-- reconciler's re-submission apply it to the engine; PATCH /tasks/{id}/files
-- rewrites it so the column always holds the selection dl-tool believes
-- the task should have.
ALTER TABLE tasks ADD COLUMN select_files TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN select_files;
