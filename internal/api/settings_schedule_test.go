package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/L-K-M/dl-tool/internal/store"
)

// scheduleGrid builds a 168-cell grid cycling 0,1,2 so every wire value
// is present.
func scheduleGrid() []int {
	cells := make([]int, 168)
	for i := range cells {
		cells[i] = i % 3
	}

	return cells
}

// decodeScheduleBody decodes the schedule response body.
func decodeScheduleBody(t *testing.T, recorder *httptest.ResponseRecorder) ScheduleBody {
	t.Helper()

	var body ScheduleBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode schedule body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// getSchedule calls GET /settings/schedule with the test bearer
// credential.
func getSchedule(t *testing.T, env *settingsTestEnv) ScheduleBody {
	t.Helper()

	recorder := env.api.Get("/settings/schedule", "Authorization: Bearer "+env.bearer)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /settings/schedule status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	return decodeScheduleBody(t, recorder)
}

// putSchedule calls PUT /settings/schedule with the test bearer
// credential.
func putSchedule(t *testing.T, env *settingsTestEnv, body any) *httptest.ResponseRecorder {
	t.Helper()

	return env.api.Put("/settings/schedule", body, "Authorization: Bearer "+env.bearer)
}

// storedSchedule reads the grid straight from the store, so the
// transaction assertions observe the table rather than the API's own
// rendering of it.
func storedSchedule(t *testing.T, env *settingsTestEnv) [168]store.ScheduleMode {
	t.Helper()

	cells, err := store.NewSettingsStore(env.db).Schedule(t.Context())
	if err != nil {
		t.Fatalf("read stored schedule: %v", err)
	}

	return cells
}

// activeModeAt renders the wire cell of the current local hour as the
// stored-mode string the API reports in active_mode.
func activeModeAt(now time.Time, cells []int) string {
	return map[int]string{
		0: string(store.ScheduleNoDownload),
		1: string(store.ScheduleDefault),
		2: string(store.ScheduleAlternative),
	}[cells[activeScheduleIndex(now)]]
}

// assertActiveMode checks the reported active_mode against the cell the
// current hour addresses, tolerating an hour rollover between the
// response's render and this assertion.
func assertActiveMode(t *testing.T, reported string, cells []int) {
	t.Helper()

	now := time.Now()
	current := activeModeAt(now, cells)
	previous := activeModeAt(now.Add(-time.Hour), cells)
	if reported != current && reported != previous {
		t.Errorf("active_mode = %q, want %q (or %q across an hour rollover)", reported, current, previous)
	}
}

// TestScheduleRoundTrips pins the acceptance criterion: a grid holding
// all three cell values comes back identical through PUT then GET, the
// enabled flag round-trips with it, and the credential gate of doc 05
// section 1.2 applies.
func TestScheduleRoundTrips(t *testing.T) {
	env := newSettingsTestEnv(t)

	cells := scheduleGrid()
	recorder := putSchedule(t, env, map[string]any{"enabled": true, "cells": cells})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT /settings/schedule status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	put := decodeScheduleBody(t, recorder)
	if diff := cmp.Diff(cells, put.Cells); diff != "" {
		t.Errorf("PUT response cells mismatch (-want +got):\n%s", diff)
	}
	if !put.Enabled {
		t.Error("PUT response enabled = false, want true")
	}
	assertActiveMode(t, put.ActiveMode, cells)

	got := getSchedule(t, env)
	if diff := cmp.Diff(cells, got.Cells); diff != "" {
		t.Errorf("GET cells mismatch (-want +got):\n%s", diff)
	}
	if !got.Enabled {
		t.Error("GET enabled = false after PUT enabled:true, want true")
	}
	assertActiveMode(t, got.ActiveMode, cells)

	if response := env.api.Get("/settings/schedule"); response.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /settings/schedule status = %d, want 401", response.Code)
	}
}

// TestWrongLengthRejected pins the boundary: 167 and 169 cells are 422
// /problems/validation-failed.
func TestWrongLengthRejected(t *testing.T) {
	env := newSettingsTestEnv(t)

	for _, length := range []int{167, 169} {
		recorder := putSchedule(t, env, map[string]any{"enabled": true, "cells": make([]int, length)})
		problem := assertProblem(t, recorder, http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) == 0 {
			t.Errorf("length %d: problem carries no errors entries", length)
		}
	}
}

// TestCellOutOfRangeRejected pins the range rule: a value of 3 and of -1
// are 422, with an errors entry locating the offending index.
func TestCellOutOfRangeRejected(t *testing.T) {
	env := newSettingsTestEnv(t)

	for _, value := range []int{3, -1} {
		cells := scheduleGrid()
		cells[42] = value
		recorder := putSchedule(t, env, map[string]any{"enabled": true, "cells": cells})
		problem := assertProblem(t, recorder, http.StatusUnprocessableEntity, SlugValidationFailed)

		want := "cells[42]"
		located := false
		for _, entry := range problem.Errors {
			if strings.Contains(entry.Location, want) {
				located = true
			}
		}
		if !located {
			t.Errorf("value %d: no errors entry locates %q: %+v", value, want, problem.Errors)
		}
	}
}

// TestRejectedPutLeavesGridUnchanged pins the transaction guarantee: a
// rejected PUT writes nothing — every stored row and the enabled flag
// survive exactly as they were.
func TestRejectedPutLeavesGridUnchanged(t *testing.T) {
	env := newSettingsTestEnv(t)

	cells := scheduleGrid()
	if recorder := putSchedule(t, env, map[string]any{"enabled": true, "cells": cells}); recorder.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	before := storedSchedule(t, env)

	for name, body := range map[string]any{
		"short": map[string]any{"enabled": false, "cells": make([]int, 167)},
		"out-of-range": func() map[string]any {
			bad := scheduleGrid()
			bad[0] = 3
			return map[string]any{"enabled": false, "cells": bad}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			assertProblem(t, putSchedule(t, env, body), http.StatusUnprocessableEntity, SlugValidationFailed)

			if after := storedSchedule(t, env); after != before {
				t.Error("rejected PUT changed stored cells")
			}
			enabled, err := store.NewSettingsStore(env.db).ScheduleEnabled(t.Context())
			if err != nil {
				t.Fatalf("read schedule_enabled: %v", err)
			}
			if !enabled {
				t.Error("rejected PUT changed schedule_enabled to false")
			}
		})
	}
}

// TestTimezoneReported pins the read-only metadata: both responses carry
// the container zone and the cell in force, and a client-sent timezone
// or active_mode is ignored, never echoed.
func TestTimezoneReported(t *testing.T) {
	env := newSettingsTestEnv(t)

	got := getSchedule(t, env)
	if got.Timezone == "" {
		t.Fatal("GET timezone is empty")
	}
	if got.Timezone != localZoneName() {
		t.Errorf("GET timezone = %q, want the container zone %q", got.Timezone, localZoneName())
	}
	if name := localZoneName(); name == "Local" {
		t.Errorf("localZoneName() = %q, want an IANA name", name)
	}
	// A fresh grid is all-default, so the cell in force is default.
	if got.ActiveMode != string(store.ScheduleDefault) {
		t.Errorf("GET active_mode = %q on a seeded grid, want %q", got.ActiveMode, store.ScheduleDefault)
	}

	cells := scheduleGrid()
	recorder := putSchedule(t, env, map[string]any{
		"enabled":     false,
		"cells":       cells,
		"timezone":    "Mars/Olympus_Mons",
		"active_mode": string(store.ScheduleNoDownload),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	put := decodeScheduleBody(t, recorder)
	if put.Timezone != localZoneName() {
		t.Errorf("PUT echoed a client timezone %q, want the container zone %q", put.Timezone, localZoneName())
	}
	assertActiveMode(t, put.ActiveMode, cells)
	if put.Enabled {
		t.Error("PUT response enabled = true after enabled:false, want false")
	}
	if stored, err := store.NewSettingsStore(env.db).ScheduleEnabled(t.Context()); err != nil || stored {
		t.Errorf("stored schedule_enabled = %v, %v; want false, nil", stored, err)
	}
}

// TestReplaceScheduleMissingRowFails pins the write-side counterpart of
// the strict 168-row read: a table missing a cell fails the replace
// loudly, and the transaction leaves both the grid and the flag at their
// last committed values.
func TestReplaceScheduleMissingRowFails(t *testing.T) {
	env := newSettingsTestEnv(t)

	cells := scheduleGrid()
	if recorder := putSchedule(t, env, map[string]any{"enabled": true, "cells": cells}); recorder.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if _, err := env.db.ExecContext(t.Context(), `DELETE FROM bandwidth_schedule WHERE day = 3 AND hour = 10`); err != nil {
		t.Fatalf("delete schedule cell: %v", err)
	}

	var modes [168]store.ScheduleMode
	for i := range modes {
		modes[i] = store.ScheduleAlternative
	}
	settings := store.NewSettingsStore(env.db)
	if err := settings.ReplaceSchedule(t.Context(), false, modes); err == nil {
		t.Fatal("ReplaceSchedule against a 167-row table error = nil, want failure")
	}

	// The rolled-back transaction leaves the flag at its last committed
	// value and a surviving row at its stored mode.
	enabled, err := settings.ScheduleEnabled(t.Context())
	if err != nil {
		t.Fatalf("read schedule_enabled: %v", err)
	}
	if !enabled {
		t.Error("rolled-back ReplaceSchedule changed schedule_enabled to false")
	}
	var mode string
	if err := env.db.GetContext(t.Context(), &mode, `SELECT mode FROM bandwidth_schedule WHERE day = 0 AND hour = 0`); err != nil {
		t.Fatalf("read surviving cell: %v", err)
	}
	if mode != string(store.ScheduleNoDownload) {
		t.Errorf("surviving cell mode = %q after rolled-back replace, want %q", mode, store.ScheduleNoDownload)
	}
}

// TestScheduleTranslations covers the two translation helpers the wire
// integers share with the stored enum.
func TestScheduleTranslations(t *testing.T) {
	for cell, want := range map[int]store.ScheduleMode{
		0: store.ScheduleNoDownload,
		1: store.ScheduleDefault,
		2: store.ScheduleAlternative,
	} {
		mode, err := CellToMode(cell)
		if err != nil {
			t.Fatalf("CellToMode(%d) error = %v", cell, err)
		}
		if mode != want {
			t.Errorf("CellToMode(%d) = %q, want %q", cell, mode, want)
		}
		if ModeToCell(mode) != cell {
			t.Errorf("ModeToCell(%q) = %d, want %d", mode, ModeToCell(mode), cell)
		}
	}
	if _, err := CellToMode(3); err == nil {
		t.Error("CellToMode(3) error = nil, want rejection")
	}
	if _, err := CellToMode(-1); err == nil {
		t.Error("CellToMode(-1) error = nil, want rejection")
	}
}
