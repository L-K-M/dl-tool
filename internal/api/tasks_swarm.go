// The three tracker operations of docs/05-api-contract.md section 5.9:
// GET /tasks/{id}/trackers lists a BitTorrent task's swarm sources — the
// engine's synthetic DHT, PeX and LSD rows included — POST adds tracker
// urls and DELETE removes them. Pseudo-tracker rows are listed and can
// never be removed; every added url is scheme-checked and run past the
// block list of docs/12-security-and-threat-model.md section 2.1, so a
// tracker cannot address a private host.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/qbittorrent"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListTaskTrackers  = "list-task-trackers"
	operationAddTaskTrackers   = "add-task-trackers"
	operationRemoveTaskTracker = "remove-task-tracker"
)

const (
	trackersDetailForeign       = "the engine no longer holds this task"
	trackersDetailListingFailed = "the engine's tracker listing could not be fetched"
	trackersDetailChangeFailed  = "the engine did not accept the tracker change"
	trackersDetailNotBitTorrent = "the task's engine does not expose a BitTorrent swarm"
	trackersDetailNotAdmitted   = "the task has not been handed to its engine yet"
	trackersDetailPseudoRemoval = "a pseudo-tracker (DHT, PeX, LSD) cannot be removed"
	trackersDetailNoURLs        = "the request names no tracker urls"
	trackersDetailInvalidURL    = "the tracker urls hold entries that failed validation"
	trackersDetailUnknownScheme = "a tracker url must carry a host and one of the http, https, udp, ws and wss schemes"
	trackersDetailBlockedHost   = "a tracker url resolves to an address dl-tool refuses to contact"
)

// trackerSchemes is the announce vocabulary of docs/05 section 5.9: the
// schemes a BitTorrent announce url may carry.
var trackerSchemes = map[string]struct{}{
	"http": {}, "https": {}, "udp": {}, "ws": {}, "wss": {},
}

// trackerBlockedIPv4 carries the denied IPv4 prefixes of
// docs/12-security-and-threat-model.md section 2.1, verbatim.
var trackerBlockedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// IPv6 is an allow-list, not a deny-list (doc 12 section 2.1): only
// 2000::/3 is reachable at all, and inside it the listed ranges stay
// denied.
var (
	trackerAllowedIPv6 = netip.MustParsePrefix("2000::/3")
	trackerBlockedIPv6 = []netip.Prefix{
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
		netip.MustParsePrefix("2620:4f:8000::/48"),
		netip.MustParsePrefix("3fff::/20"),
	}
)

// trackerEngine is implemented by an engine that exposes a BitTorrent
// swarm. Declaring it here, at the consumer, keeps the Engine interface
// of 06 section 1 unchanged.
type trackerEngine interface {
	Trackers(ctx context.Context, id string) ([]qbittorrent.TrackerEntry, error)
	AddTrackers(ctx context.Context, id string, urls []string) error
	RemoveTrackers(ctx context.Context, id string, urls []string) error
}

// TrackerDTO is one row of the listing: status is the engine's own value
// rendered as a string, and seeds, peers and update_timer_seconds are
// null where the engine reports no value for the row.
type TrackerDTO struct {
	URL                string `json:"url"`
	Status             string `json:"status"`
	Seeds              *int   `json:"seeds"`
	Peers              *int   `json:"peers"`
	Message            string `json:"message"`
	UpdateTimerSeconds *int   `json:"update_timer_seconds"`
}

// ListTaskTrackersInput addresses one task by id.
type ListTaskTrackersInput struct {
	ID string `path:"id" doc:"The tsk_ id of the task"`
}

// ListTaskTrackersOutput is the listing both GET and POST return.
type ListTaskTrackersOutput struct {
	Body struct {
		Trackers []TrackerDTO `json:"trackers"`
	}
}

// AddTaskTrackersInput carries the urls to add.
type AddTaskTrackersInput struct {
	ID   string `path:"id" doc:"The tsk_ id of the task"`
	Body struct {
		URLs []string `json:"urls" minItems:"1" maxItems:"100" doc:"http, https, udp, ws and wss announce urls"`
	}
}

// AddTaskTrackersOutput is the updated listing the 201 answer carries.
type AddTaskTrackersOutput struct {
	Body struct {
		Trackers []TrackerDTO `json:"trackers"`
	}
}

// RemoveTaskTrackerInput addresses one task and the urls to remove.
type RemoveTaskTrackerInput struct {
	ID  string   `path:"id" doc:"The tsk_ id of the task"`
	URL []string `query:"url,explode" doc:"The announce urls to remove; repeat the parameter for several"`
}

// RemoveTaskTrackerOutput is the bodiless 204 answer.
type RemoveTaskTrackerOutput struct{}

// ListTaskTrackers serves GET /tasks/{id}/trackers (doc 05 section 5.9):
// the engine's listing lands in task_trackers and the answer is that same
// listing, so the rows on disk are exactly the rows the response was
// built from.
func (h *TaskHandlers) ListTaskTrackers(ctx context.Context, in *ListTaskTrackersInput) (*ListTaskTrackersOutput, error) {
	task, swarm, err := h.taskWithSwarm(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	entries, err := h.refreshTrackers(ctx, task, swarm)
	if err != nil {
		return nil, err
	}

	output := &ListTaskTrackersOutput{}
	output.Body.Trackers = trackerDTOs(entries)

	return output, nil
}

// AddTaskTrackers serves POST /tasks/{id}/trackers: every url is
// validated and block-listed before any engine call, the engine adds
// them, and the answer is the full re-listed set.
func (h *TaskHandlers) AddTaskTrackers(ctx context.Context, in *AddTaskTrackersInput) (*AddTaskTrackersOutput, error) {
	task, swarm, err := h.taskWithSwarm(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	// A JSON null decodes to a nil slice that no minItems tag can tell
	// from an absent one, so this backstop — the PATCH files rule — owns
	// the case before any engine call.
	if len(in.Body.URLs) == 0 {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailNoURLs)
	}

	if fieldErrs := validateTrackerURLShapes(in.Body.URLs); len(fieldErrs) > 0 {
		return nil, trackerProblem(fieldErrs)
	}
	if trackerURLsBlocked(ctx, in.Body.URLs) {
		return nil, Problem(SlugSSRFBlocked, http.StatusForbidden, trackersDetailBlockedHost)
	}

	if err := swarm.AddTrackers(ctx, engineTaskID(task.Engine, task.EngineRef), in.Body.URLs); err != nil {
		return nil, swarmChangeProblem(err)
	}

	entries, err := h.refreshTrackers(ctx, task, swarm)
	if err != nil {
		return nil, err
	}

	output := &AddTaskTrackersOutput{}
	output.Body.Trackers = trackerDTOs(entries)

	return output, nil
}

// RemoveTaskTracker serves DELETE /tasks/{id}/trackers: the urls reach
// the engine, which refuses a pseudo-tracker without issuing the request,
// and the answer is 204 once the store holds the post-removal listing.
func (h *TaskHandlers) RemoveTaskTracker(ctx context.Context, in *RemoveTaskTrackerInput) (*RemoveTaskTrackerOutput, error) {
	task, swarm, err := h.taskWithSwarm(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	// An empty url set decodes to a nil or empty slice no schema tag can
	// distinguish from an absent parameter, so this backstop owns it.
	if len(in.URL) == 0 {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailNoURLs)
	}

	if err := swarm.RemoveTrackers(ctx, engineTaskID(task.Engine, task.EngineRef), in.URL); err != nil {
		return nil, swarmChangeProblem(err)
	}

	if _, err := h.refreshTrackers(ctx, task, swarm); err != nil {
		return nil, err
	}

	return &RemoveTaskTrackerOutput{}, nil
}

// taskWithSwarm loads the task, resolves its engine and narrows it to a
// trackerEngine: 404 for an unknown task, 503 when the task's engine is
// not registered, and 422 when the task is not a BitTorrent task — such a
// task exposes no tracker data at all, not even an empty list.
func (h *TaskHandlers) taskWithSwarm(ctx context.Context, id string) (store.Task, trackerEngine, error) {
	task, e, err := h.taskWithEngine(ctx, id)
	if err != nil {
		return store.Task{}, nil, err
	}

	if !hasCapability(e, engine.CapBitTorrent) {
		return store.Task{}, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailNotBitTorrent)
	}
	swarm, ok := e.(trackerEngine)
	if !ok {
		// A registered engine that declares bittorrent but carries no
		// swarm methods cannot serve the operation; the honest answer is
		// the same 422 the capability gate gives.
		return store.Task{}, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailNotBitTorrent)
	}
	if task.EngineRef == nil {
		// No engine holds the transfer yet, so no swarm exists to list.
		return store.Task{}, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailNotAdmitted)
	}

	return task, swarm, nil
}

// refreshTrackers fetches the engine's listing and mirrors it into
// task_trackers, so the table holds exactly the rows of the last
// successful listing. An engine that no longer holds the torrent answers
// 404; every other engine failure is the 503 — unlike the files listing
// there is no documented store-backed answer for trackers.
func (h *TaskHandlers) refreshTrackers(ctx context.Context, task store.Task, swarm trackerEngine) ([]qbittorrent.TrackerEntry, error) {
	entries, err := swarm.Trackers(ctx, engineTaskID(task.Engine, task.EngineRef))
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			return nil, Problem(SlugNotFound, http.StatusNotFound, trackersDetailForeign)
		}

		return nil, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, trackersDetailListingFailed)
	}

	rows := make([]store.Tracker, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, store.Tracker{
			URL:                entry.URL,
			Status:             entry.Status,
			UpdateTimerSeconds: entry.UpdateTimerSeconds,
			Seeds:              entry.Seeds,
			Peers:              entry.Peers,
			Message:            entry.Message,
		})
	}
	if err := h.tasks.ReplaceTrackers(ctx, task.ID, rows); err != nil {
		return nil, internalFailure(ctx, "replace task trackers", err)
	}

	return entries, nil
}

// swarmChangeProblem maps one engine mutation failure: ErrNotSupported is
// the pseudo-tracker refusal, everything else is the 503 of a daemon that
// could not take the change.
func swarmChangeProblem(err error) error {
	if errors.Is(err, engine.ErrNotSupported) {
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailPseudoRemoval)
	}

	return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, trackersDetailChangeFailed)
}

// trackerDTOs renders the engine listing as the wire listing.
func trackerDTOs(entries []qbittorrent.TrackerEntry) []TrackerDTO {
	trackers := make([]TrackerDTO, 0, len(entries))
	for _, entry := range entries {
		trackers = append(trackers, TrackerDTO{
			URL:                entry.URL,
			Status:             entry.Status,
			Seeds:              entry.Seeds,
			Peers:              entry.Peers,
			Message:            entry.Message,
			UpdateTimerSeconds: entry.UpdateTimerSeconds,
		})
	}

	return trackers
}

// validateTrackerURLShapes checks every added url's shape: the scheme
// must be one of the announce vocabulary's and the url must carry a
// host. The per-url reason rides errors[] with its location, and the
// check runs before any engine call.
func validateTrackerURLShapes(urls []string) []*huma.ErrorDetail {
	fieldErrs := make([]*huma.ErrorDetail, 0)
	for i, raw := range urls {
		location := fmt.Sprintf("body.urls[%d]", i)

		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message: trackersDetailUnknownScheme, Location: location,
			})

			continue
		}
		if _, ok := trackerSchemes[u.Scheme]; !ok {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message: trackersDetailUnknownScheme, Location: location,
			})
		}
	}

	return fieldErrs
}

// trackerURLsBlocked reports whether any added url addresses a host the
// block list denies. It runs after the shape check, so a refused url is
// always one a tracker could legally carry.
func trackerURLsBlocked(ctx context.Context, urls []string) bool {
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			// Unreachable past the shape check; failing closed keeps this
			// function total on its own.
			return true
		}
		if trackerHostBlocked(ctx, u.Hostname()) {
			return true
		}
	}

	return false
}

// trackerProblem renders the collected field errors as one 422 whose
// errors[] carries each url's reason at its location.
func trackerProblem(fieldErrs []*huma.ErrorDetail) error {
	problem := Problem(SlugValidationFailed, http.StatusUnprocessableEntity, trackersDetailInvalidURL)
	var model *huma.ErrorModel
	// Problem builds an *huma.ErrorModel by construction; the guard keeps
	// a future change to that return type from turning every invalid add
	// into a nil-pointer 500.
	if !errors.As(problem, &model) {
		return problem
	}
	model.Errors = fieldErrs

	return problem
}

// trackerIPBlocked applies the block list of doc 12 section 2.1 to one
// address. An IPv4-mapped IPv6 address unmapps first, so
// ::ffff:169.254.169.254 hits the IPv4 rules.
func trackerIPBlocked(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}

	if ip.Is4() {
		for _, prefix := range trackerBlockedIPv4 {
			if prefix.Contains(ip) {
				return true
			}
		}

		return false
	}

	if !trackerAllowedIPv6.Contains(ip) {
		return true
	}
	for _, prefix := range trackerBlockedIPv6 {
		if prefix.Contains(ip) {
			return true
		}
	}

	return false
}

// trackerHostBlocked reports whether host addresses a blocked address. A
// literal address is judged directly; a name is resolved and judged on
// every answer, so any private record refuses it. A name that does not
// resolve is allowed: the daemon resolves again at announce time, this
// gate is advisory against private hosts, not a liveness probe, and a
// temporarily unresolvable tracker would otherwise be unaddable.
func trackerHostBlocked(ctx context.Context, host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return trackerIPBlocked(ip)
	}

	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		// The reason is logged without the host: an announce url can carry
		// a secret in its userinfo.
		logFromContext(ctx).Warn("tracker host could not be resolved; the block check allows it",
			slog.String("reason", dnsErrorText(err)))

		return false
	}
	for _, addr := range addrs {
		if trackerIPBlocked(addr) {
			return true
		}
	}

	return false
}

// dnsErrorText renders a resolver failure, or the no-addresses case a nil
// error reports.
func dnsErrorText(err error) string {
	if err == nil {
		return "no addresses"
	}

	return err.Error()
}
