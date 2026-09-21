package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationGetSchedule = "get-schedule"
	operationPutSchedule = "put-schedule"
)

// ScheduleBody is the wire shape of doc 05 section 11.2. Cells are
// integers, not mode strings: 0 = no download, 1 = default speed,
// 2 = alternative speed.
type ScheduleBody struct {
	Enabled bool  `json:"enabled"`
	Cells   []int `json:"cells" minItems:"168" maxItems:"168" minimum:"0" maximum:"2" nullable:"false" doc:"168 cells indexed day*24+hour, day 0 = Monday"`
	// omitempty keeps the read-only members out of the PUT schema's
	// required list so a typed client needs only enabled and cells; both
	// are still emitted on every response.
	Timezone   string `json:"timezone,omitempty" readOnly:"true" doc:"IANA name of the zone the cells are evaluated in"`
	ActiveMode string `json:"active_mode,omitempty" readOnly:"true" enum:"no_download,default,alternative" doc:"the cell in force at the moment of the call"`
}

// GetScheduleOutput is the GET /settings/schedule body.
type GetScheduleOutput struct{ Body ScheduleBody }

// PutScheduleInput carries the wholesale replacement of PUT
// /settings/schedule — the same body shape the GET answers.
type PutScheduleInput struct{ Body ScheduleBody }

// PutScheduleOutput returns the stored grid after the replace.
type PutScheduleOutput struct{ Body ScheduleBody }

// registerScheduleOperations mounts get-schedule and put-schedule on the
// Huma API; Server.registerOperations is the call site.
func (h *SettingsHandlers) registerScheduleOperations(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationGetSchedule,
		Method:      http.MethodGet,
		Path:        "/settings/schedule",
		Summary:     "Read the 24x7 bandwidth schedule",
		Description: "The 168-cell grid and its enabled flag, plus the container time zone the cells are evaluated in and the cell in force at the moment of the call.",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
	}, h.GetSchedule)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPutSchedule,
		Method:      http.MethodPut,
		Path:        "/settings/schedule",
		Summary:     "Replace the 24x7 bandwidth schedule",
		Description: "Replaces all 168 cells and the enabled flag in one transaction and answers the stored grid. A body that is not exactly 168 integers in 0..2 is 422 and nothing is written. timezone and active_mode are read-only: values a client sends are ignored.",
		Tags:        []string{"settings"},
		Security:    credentialRequired,
	}, h.PutSchedule)
}

// GetSchedule serves GET /settings/schedule (doc 05 section 11.2).
func (h *SettingsHandlers) GetSchedule(ctx context.Context, _ *struct{}) (*GetScheduleOutput, error) {
	body, err := h.scheduleBody(ctx)
	if err != nil {
		return nil, err
	}

	return &GetScheduleOutput{Body: body}, nil
}

// PutSchedule serves PUT /settings/schedule: the schema has already
// enforced exactly 168 integers in 0..2, so the length and range checks
// below are the second line, and the replace is one transaction — a
// rejected body cannot leave a partial grid. The answer re-reads the
// stored grid rather than echoing the request.
func (h *SettingsHandlers) PutSchedule(ctx context.Context, in *PutScheduleInput) (*PutScheduleOutput, error) {
	var cells [168]store.ScheduleMode
	if len(in.Body.Cells) != len(cells) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "cells must hold exactly 168 entries")
	}
	for i, cell := range in.Body.Cells {
		mode, err := CellToMode(cell)
		if err != nil {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, err.Error())
		}
		cells[i] = mode
	}

	if err := h.settings.ReplaceSchedule(ctx, in.Body.Enabled, cells); err != nil {
		return nil, internalFailure(ctx, "replace schedule", err)
	}

	body, err := h.scheduleBody(ctx)
	if err != nil {
		return nil, err
	}

	return &PutScheduleOutput{Body: body}, nil
}

// scheduleBody renders the stored grid into the wire shape: the
// schedule_enabled flag, the cells as wire integers, the container's TZ
// name — the one zone the cells are evaluated in, never a client-sent
// value — and the cell in force at the moment of the call. Until T110's
// evaluation lands, that active cell is the cell of the current local
// hour.
func (h *SettingsHandlers) scheduleBody(ctx context.Context) (ScheduleBody, error) {
	modes, enabled, err := h.settings.ScheduleSnapshot(ctx)
	if err != nil {
		return ScheduleBody{}, internalFailure(ctx, "read schedule", err)
	}

	body := ScheduleBody{
		Enabled:    enabled,
		Cells:      make([]int, len(modes)),
		Timezone:   localZoneName(),
		ActiveMode: string(modes[activeScheduleIndex(time.Now())]),
	}
	for i, mode := range modes {
		body.Cells[i] = ModeToCell(mode)
	}

	return body, nil
}

// localZoneName reports the zone the cells are evaluated in as an IANA
// name. time.Local.String() is the zone name whenever TZ is set — the
// documented container configuration — but "Local" when the process
// loaded /etc/localtime without learning its name; the zoneinfo symlink
// recovers that name, and the container's unset-TZ zone (UTC) is the
// fallback.
func localZoneName() string {
	if name := time.Local.String(); name != "Local" {
		return name
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if name, ok := strings.CutPrefix(target, "/usr/share/zoneinfo/"); ok {
			return name
		}
	}

	return "UTC"
}

// activeScheduleIndex resolves the grid index the current local hour
// addresses: day*24+hour with Monday as day 0. T110 owns the DST
// repeated- and skipped-hour rules; until it lands, the cell of the
// current hour stands in for the cell in force.
func activeScheduleIndex(now time.Time) int {
	day := (int(now.Weekday()) + 6) % 7

	return day*24 + now.Hour()
}

// CellToMode translates one wire integer of doc 05 section 11.2 into the
// stored mode: 0 = no download, 1 = default speed, 2 = alternative speed.
func CellToMode(c int) (store.ScheduleMode, error) {
	switch c {
	case 0:
		return store.ScheduleNoDownload, nil
	case 1:
		return store.ScheduleDefault, nil
	case 2:
		return store.ScheduleAlternative, nil
	}

	return "", fmt.Errorf("cell value %d is outside 0..2", c)
}

// ModeToCell renders one stored mode as its wire integer. Schedule
// already rejects a stored mode outside the documented vocabulary, so
// the default branch is ScheduleDefault alone.
func ModeToCell(m store.ScheduleMode) int {
	switch m {
	case store.ScheduleNoDownload:
		return 0
	case store.ScheduleAlternative:
		return 2
	default:
		return 1
	}
}
