// The /system operations of docs/05-api-contract.md section 13. T091 adds
// POST /system/backup; GET /system/info (T092) and GET /system/logs (T096)
// join this file.
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/store"
)

// CreateBackupOutput is 201 on success. There is no request body.
type CreateBackupOutput struct {
	Status int `json:"-"`
	Body   struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
		CreatedAt string `json:"created_at"`
	}
}

// SystemHandlers serves the /system operations of doc 05 section 13.
type SystemHandlers struct {
	maintenance *store.MaintenanceStore
	backupDir   string
}

// NewSystemHandlers takes the one MaintenanceStore and the backup directory —
// filepath.Join(cfg.ConfigDir, store.BackupsDirName), resolved once.
func NewSystemHandlers(m *store.MaintenanceStore, backupDir string) *SystemHandlers {
	return &SystemHandlers{maintenance: m, backupDir: backupDir}
}

// CreateBackup runs VACUUM INTO into the backup directory and prunes to the
// newest store.BackupKeepCount snapshots — the same work the nightly cron
// entry does.
func (h *SystemHandlers) CreateBackup(ctx context.Context, _ *struct{}) (*CreateBackupOutput, error) {
	result, err := h.maintenance.BackupInto(ctx, h.backupDir)
	if errors.Is(err, store.ErrBackupRunning) {
		return nil, Problem(SlugConflict, http.StatusConflict, "a backup is already running")
	}
	if err != nil {
		return nil, internalFailure(ctx, "create backup", err)
	}
	if _, err := h.maintenance.PruneBackups(ctx, h.backupDir, store.BackupKeepCount); err != nil {
		return nil, internalFailure(ctx, "prune backups", err)
	}

	output := &CreateBackupOutput{Status: http.StatusCreated}
	output.Body.Path = result.Path
	output.Body.SizeBytes = result.SizeBytes
	output.Body.CreatedAt = result.CreatedAt.UTC().Format(time.RFC3339)

	return output, nil
}

// Register mounts POST /system/backup on the Huma API.
func (h *SystemHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID:   "create-backup",
		Method:        http.MethodPost,
		Path:          "/system/backup",
		DefaultStatus: http.StatusCreated,
		Summary:       "Back up the database",
		Description:   "Writes a consistent snapshot with VACUUM INTO and returns its path and size, retaining the newest seven files. 409 /problems/conflict while another backup is running; 500 /problems/internal on any other failure.",
		Security:      credentialRequired,
	}, h.CreateBackup)
}
