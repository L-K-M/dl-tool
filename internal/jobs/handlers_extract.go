package jobs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The failure codes of docs/04-data-model.md section 4.2 the extractor
// maps onto; extract_failed is the fallback for everything unclassified.
const (
	codeExtractFailed              = "extract_failed"
	codeExtractFailedWrongPassword = "extract_failed_wrong_password"
	codeExtractFailedInvalid       = "extract_failed_invalid_archive"
	codeExtractFailedQuota         = "extract_failed_quota_reached"
	codeExtractFailedDiskFull      = "extract_failed_disk_full"
)

var (
	// ErrInvalidArchive marks a member that fails pass 1 — a hostile name,
	// a link or a special file — or an archive 7zz cannot even list.
	ErrInvalidArchive = errors.New("jobs: archive member rejected")
	// ErrCapExceeded marks a breach of the doc 12 section 4.1 caps, checked
	// against declared sizes in pass 1 and against bytes written in pass 2.
	ErrCapExceeded = errors.New("jobs: extraction cap exceeded")
	// ErrWrongPassword marks an encrypted archive that opened with no
	// candidate; T075 supplies the candidate list.
	ErrWrongPassword = errors.New("jobs: no candidate password opened the archive")
)

// The doc 12 section 4.1 defaults. TotalUncompressedBytes is the odd one
// out: it derives from the archive's own size, so it is computed per job.
const (
	defaultMemberCount       = 10_000
	defaultSingleMemberBytes = int64(4 << 30)
	defaultMemberDepth       = 32
	defaultWallClock         = 30 * time.Minute
	maxTotalUncompressed     = int64(20 << 30)
	totalUncompressedFactor  = 10
)

// extractPollInterval is how often pass 2 samples the bytes written into
// the staging directory; tests shrink it so progress ticks are observable
// on small fixtures.
var extractPollInterval = time.Second

// Caps are the limits of doc 12 section 4.1. Zero means the documented
// default.
type Caps struct {
	TotalUncompressedBytes int64         // min(10 * archive size, 20 GiB)
	MemberCount            int           // 10000
	SingleMemberBytes      int64         // 4 GiB
	Depth                  int           // 32
	WallClock              time.Duration // 30 * time.Minute
}

func (c Caps) withDefaults(archiveSize int64) Caps {
	if c.TotalUncompressedBytes == 0 {
		c.TotalUncompressedBytes = archiveSize * totalUncompressedFactor
		if c.TotalUncompressedBytes > maxTotalUncompressed {
			c.TotalUncompressedBytes = maxTotalUncompressed
		}
	}
	if c.MemberCount == 0 {
		c.MemberCount = defaultMemberCount
	}
	if c.SingleMemberBytes == 0 {
		c.SingleMemberBytes = defaultSingleMemberBytes
	}
	if c.Depth == 0 {
		c.Depth = defaultMemberDepth
	}
	if c.WallClock == 0 {
		c.WallClock = defaultWallClock
	}

	return c
}

// Member is one row of pass 1: `7zz l -slt -y -ba <archive>`.
type Member struct {
	Path       string // as declared by the archive
	Size       int64  // declared uncompressed size
	Attributes string // raw Attributes value; a link, device, FIFO, setuid or setgid member is rejected
}

// unixModeLink is synthesised into Attributes when the archive declares a
// link through the dedicated `Symbolic Link`, `Hard Link` or `Copy Link`
// fields (rar, 7z, tar) instead of carrying a unix mode (zip): Member has
// no fourth field, so the link bit rides inside the raw attributes string
// exactly as a unix-format archive would report it.
const unixModeLink = "lrwxrwxrwx"

// ListMembers runs pass 1 and returns the declared members. It writes
// nothing. A non-zero exit — a truncated, encrypted or unreadable file —
// is ErrWrongPassword when 7zz says so and ErrInvalidArchive otherwise;
// the row set is only trustworthy on a clean run.
func ListMembers(ctx context.Context, sevenzipPath, archivePath string) ([]Member, error) {
	// The member list is streamed: a hostile archive's listing can be far
	// larger than the cap Validate enforces, so buffering it wholesale
	// would make the listing itself the memory bomb.
	cmd := exec.CommandContext(ctx, sevenzipPath, "l", "-slt", "-y", "-ba", archivePath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("jobs: pipe extractor listing: %w", err)
	}
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("jobs: start extractor listing: %w", err)
	}

	members, parseErr := parseMembers(stdout)
	if parseErr != nil {
		// The listing was cut short — a member-count bomb trips the parse
		// bound mid-stream — so the child still holds the pipe and must be
		// killed and reaped before this returns.
		if killErr := cmd.Process.Kill(); killErr != nil {
			parseErr = errors.Join(parseErr, fmt.Errorf("jobs: kill extractor listing: %w", killErr))
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			parseErr = errors.Join(parseErr, fmt.Errorf("jobs: reap extractor listing: %w", waitErr))
		}

		return nil, parseErr
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		// A cancelled context (worker shutdown, job cancel) is not a
		// verdict on the archive: surface it so the job is retried instead
		// of recorded as permanently invalid.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// A listing that fails is a verdict on the archive itself — a
		// truncated, corrupt or unreadable file — never a retryable
		// condition: invalid unless the tool names a password.
		if diagnosticMatch(string(stderr.Bytes()), "wrong password") ||
			diagnosticMatch(string(stderr.Bytes()), "cannot open encrypted archive") {
			return nil, fmt.Errorf("%w: %s", ErrWrongPassword, firstLine(string(stderr.Bytes())))
		}

		return nil, fmt.Errorf("%w: %s", ErrInvalidArchive, firstLine(string(stderr.Bytes())))
	}

	return members, nil
}

// parseMemberBound bounds the members parseMembers will buffer before
// Validate's count cap can look at them — ten times the enforced cap is
// far past any real archive and far short of a memory problem.
// maxMemberLineBytes bounds one `Key = Value` line of the listing.
const (
	parseMemberBound   = defaultMemberCount * 10
	maxMemberLineBytes = 1 << 20
)

// parseMembers reads the -slt record format: blocks of `Key = Value` lines
// separated by blank lines, one block per member (-ba suppresses the
// archive header block).
func parseMembers(out io.Reader) ([]Member, error) {
	var members []Member
	var member *Member
	var link bool

	flush := func() {
		if member == nil {
			return
		}
		if link {
			member.Attributes = strings.TrimSpace(member.Attributes + " " + unixModeLink)
		}
		members = append(members, *member)
		member, link = nil, false
	}

	// A member line can hold a hostile multi-kilobyte path; bound the line,
	// not just the member count.
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64<<10), maxMemberLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			flush()
			continue
		}
		key, value, found := strings.Cut(line, " = ")
		if !found {
			continue
		}
		if key == "Path" {
			flush()
			member = &Member{Path: value}
			// The listing is read before Validate can apply the member cap,
			// so the parse itself stops at an order of magnitude past it —
			// a hostile archive cannot make the listing the memory bomb.
			if len(members)+1 > parseMemberBound {
				return nil, fmt.Errorf("%w: more than %d members listed", ErrCapExceeded, parseMemberBound)
			}
			continue
		}
		if member == nil {
			continue
		}
		switch key {
		case "Size":
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			size, err := strconv.ParseInt(trimmed, 10, 64)
			if err != nil || size < 0 {
				return nil, fmt.Errorf("%w: member %q declares unparsable size %q",
					ErrInvalidArchive, member.Path, value)
			}
			member.Size = size
		case "Attributes", "Mode":
			// tar-style archives report the unix mode as Mode, zip-style
			// ones as Attributes; both feed memberHostile's token scan.
			member.Attributes = strings.TrimSpace(member.Attributes + " " + value)
		case "Symbolic Link", "Hard Link", "Copy Link":
			if strings.TrimSpace(value) != "" && strings.TrimSpace(value) != "-" {
				link = true
			}
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: listing read failed: %s", ErrInvalidArchive, err)
	}

	return members, nil
}

// Validate applies pass 1's rejection rules to members against the default
// caps. It returns ErrInvalidArchive for a bad member name and
// ErrCapExceeded for a cap breach.
func Validate(members []Member, archiveSize int64) error {
	return validate(members, Caps{}.withDefaults(archiveSize))
}

func validate(members []Member, caps Caps) error {
	if len(members) > caps.MemberCount {
		return fmt.Errorf("%w: %d members exceed the %d cap", ErrCapExceeded, len(members), caps.MemberCount)
	}

	var total int64
	for _, m := range members {
		if err := validateMemberName(m.Path, caps); err != nil {
			return err
		}
		if memberHostile(m.Attributes) {
			return fmt.Errorf("%w: %q is a link, special file or privileged mode", ErrInvalidArchive, m.Path)
		}
		if m.Size > caps.SingleMemberBytes {
			return fmt.Errorf("%w: %q declares %d bytes over the %d cap",
				ErrCapExceeded, m.Path, m.Size, caps.SingleMemberBytes)
		}
		total += m.Size
		if total > caps.TotalUncompressedBytes {
			return fmt.Errorf("%w: members declare %d bytes over the %d cap",
				ErrCapExceeded, total, caps.TotalUncompressedBytes)
		}
	}

	return nil
}

// validateMemberName rejects the hostile shapes doc 12 section 4.1 lists:
// an absolute path, a `..` element, a depth past the cap, or a segment
// fsx.SanitiseSegment would rewrite — a name that needs sanitising is
// refused, never silently renamed, so the written tree matches the listing
// byte for byte.
func validateMemberName(name string, caps Caps) error {
	if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return fmt.Errorf("%w: member path %q is empty or absolute", ErrInvalidArchive, name)
	}

	depth := 0
	for _, segment := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			return fmt.Errorf("%w: member path %q contains a .. element", ErrInvalidArchive, name)
		}
		if fsx.SanitiseSegment(segment) != segment {
			return fmt.Errorf("%w: member path %q fails sanitisation", ErrInvalidArchive, name)
		}
		depth++
	}
	if depth == 0 || depth > caps.Depth {
		return fmt.Errorf("%w: member path %q depth %d outside 1..%d", ErrInvalidArchive, name, depth, caps.Depth)
	}

	return nil
}

// memberHostile reports whether the member's declared attributes describe
// anything but a plain file or directory — a link, device, FIFO, socket,
// or a setuid/setgid mode. The unix mode is the only token checked: the
// first character is the file kind and positions 3 and 6 carry setuid and
// setgid.
func memberHostile(attributes string) bool {
	for _, field := range strings.Fields(attributes) {
		if len(field) != 10 || !strings.ContainsRune("-dlpscbh", rune(field[0])) {
			continue
		}
		if field[0] != '-' && field[0] != 'd' {
			return true
		}
		if field[3] == 's' || field[3] == 'S' || field[6] == 's' || field[6] == 'S' {
			return true
		}
	}

	return false
}

// Handle runs one extract job end to end: pass 1 listing and validation,
// pass 2 extraction into a staging directory on the payload's filesystem,
// pass 3 verification and the rename into place. A recorded extraction
// failure returns nil — the task carries the error_code and the job is
// done; only an infrastructure failure that could not be recorded is
// returned for the worker to retry.
func (h *ExtractHandler) Handle(ctx context.Context, job store.Job) error {
	if job.TaskID == nil {
		return fmt.Errorf("jobs: %s job %q has no task_id", JobKindExtract, job.ID)
	}
	taskID := *job.TaskID

	task, err := h.tasks.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the row is gone; nothing left to extract
	}
	if err != nil {
		return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
	}

	switch task.State {
	case "completed":
		if err := h.tasks.Transition(ctx, taskID, "extracting", eventExtractStarted, "extracting archive payload"); err != nil {
			return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
		}
	case "extracting":
		// A crash or a duplicate claim re-enters mid-run; the row is
		// already extracting, so start the pass sequence over.
	default:
		return nil // error, removed, or otherwise not ours to touch
	}

	if err := h.tasks.SetExtractProgress(ctx, taskID, 0); err != nil {
		return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
	}

	if task.ContentPath == nil || *task.ContentPath == "" {
		return h.fail(ctx, taskID, fmt.Errorf("%w: task carries no content_path", ErrInvalidArchive))
	}
	archivePath := *task.ContentPath

	runErr := h.run(ctx, taskID, archivePath)
	if runErr == nil {
		if err := h.tasks.SetExtractProgress(ctx, taskID, 100); err != nil {
			return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
		}
		if err := h.tasks.Transition(ctx, taskID, "completed", eventExtractCompleted, "archive extracted"); err != nil {
			return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
		}

		return nil
	}

	// A cancelled context is the pool shutting down, not an extraction
	// verdict: leave the row extracting and let the rescheduled job retry.
	if ctx.Err() != nil {
		return fmt.Errorf("jobs: extract task %q: %w", taskID, ctx.Err())
	}

	return h.fail(ctx, taskID, runErr)
}

// run is the three-pass recipe of doc 12 section 4.1. Staging directories
// sit inside the payload's own directory — the target data root — so a
// pass-1 escape writes inside the root and the final move is a rename.
func (h *ExtractHandler) run(ctx context.Context, taskID, archivePath string) error {
	info, err := os.Lstat(archivePath)
	if err != nil {
		return fmt.Errorf("jobs: stat archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %q is not a regular file", ErrInvalidArchive, archivePath)
	}

	caps := h.caps.withDefaults(info.Size())
	root := filepath.Dir(archivePath)

	// A crashed run leaves its staging dir behind; only a dir older than
	// any run could possibly still be live may be reaped — the bound is a
	// fixed multiple of the wall clock because a staging root's mtime
	// stops advancing once 7zz is writing deep inside it.
	sweepStaleStaging(root, staleStagingAge)

	members, err := ListMembers(ctx, h.sevenzipPath, archivePath)
	if err != nil {
		return err
	}

	// A gzip wrapper's single member is the container layer, not content —
	// the total-size cap lands on the inner pass instead, and the outer
	// member is bounded by the single-member cap and the write poll.
	outerCaps := caps
	if gzipWrapped(archivePath) && len(members) == 1 && archiveExt(members[0].Path) != "" {
		outerCaps.TotalUncompressedBytes = caps.SingleMemberBytes
	}
	if err := validate(members, outerCaps); err != nil {
		return err
	}

	tmp, err := h.extractPass(ctx, taskID, archivePath, root, sumMemberBytes(members), outerCaps)
	if err != nil {
		return err
	}

	// A gzip outer layer unwraps exactly one level: a .tgz's first pass
	// yields the inner .tar, which gets the same three passes — that is
	// the archive's own container, not a nested archive to recurse into.
	if gzipWrapped(archivePath) {
		if inner := soleInnerArchive(tmp); inner != "" {
			// Each container layer gets its own budget: the cap is a
			// multiple of the archive being unpacked, and a .tgz's inner
			// .tar is far larger than the gzip that carried it.
			innerInfo, err := os.Stat(inner)
			if err != nil {
				return fmt.Errorf("jobs: stat inner container: %w", errors.Join(err, removeStaging(tmp)))
			}
			innerCaps := h.caps.withDefaults(innerInfo.Size())

			innerMembers, err := ListMembers(ctx, h.sevenzipPath, inner)
			if err != nil {
				return errors.Join(err, removeStaging(tmp))
			}
			if err := validate(innerMembers, innerCaps); err != nil {
				return errors.Join(err, removeStaging(tmp))
			}
			innerTmp, err := h.extractPass(ctx, taskID, inner, root, sumMemberBytes(innerMembers), innerCaps)
			err = errors.Join(err, removeStaging(tmp))
			if err != nil {
				return err
			}
			tmp = innerTmp
			// The staged tree came from the inner container, so the final
			// verify enforces its caps, not the gzip wrapper's.
			caps = innerCaps
		}
	}

	target := filepath.Join(root, payloadStem(archivePath))
	if err := verifyAndMove(tmp, target, caps); err != nil {
		return errors.Join(err, removeStaging(tmp))
	}

	return nil
}

// extractPass is pass 2: `7zz x` into a fresh staging directory under
// root, with the wall-clock deadline, the process-group kill and the
// bytes-written poll of doc 12 section 4.1.
func (h *ExtractHandler) extractPass(
	ctx context.Context,
	taskID, archivePath, root string,
	declaredBytes int64,
	caps Caps,
) (string, error) {
	tmp := filepath.Join(root, ".dl-tool-extract-"+ulid.Make().String())
	// Owner-only while contents are still unverified attacker output;
	// verifyAndMove applies the delivered modes at the end.
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return "", fmt.Errorf("jobs: create extract staging dir: %w", err)
	}

	extractCtx, cancel := context.WithTimeout(ctx, caps.WallClock)
	defer cancel()

	cmd := exec.CommandContext(
		extractCtx, h.sevenzipPath,
		"x", "-y", "-bd", "-o"+tmp, "-p", archivePath,
	)
	// Setpgid plus the group kill: the deadline must take down the whole
	// decoder tree, not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// stderr gets its own capped sink: 7zz's verdict lines land there last,
	// and a head-only buffer would lose them under a flood of member names.
	var stderrBuf, stdoutBuf cappedBuffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("jobs: start extractor: %w", errors.Join(err, removeStaging(tmp)))
	}

	pollDone := h.pollProgress(ctx, extractCtx, taskID, tmp, declaredBytes, caps.TotalUncompressedBytes, cancel)
	waitErr := cmd.Wait()
	cancel()
	// Reading the closed channel drains the poll goroutine, so no progress
	// write can land after this point — the handler's own 0 and 100 are
	// then the first and last gauge values.
	pollFailure := <-pollDone

	var runErr error
	switch {
	case pollFailure != nil:
		// The poll cancelled the run: the byte cap was breached or the
		// task left the extracting state.
		runErr = pollFailure
	case ctx.Err() != nil:
		runErr = fmt.Errorf("jobs: extraction interrupted: %w", ctx.Err())
	case errors.Is(extractCtx.Err(), context.DeadlineExceeded):
		runErr = fmt.Errorf("jobs: extraction exceeded the %s wall clock", caps.WallClock)
	case waitErr != nil:
		runErr = classifySevenzipError(waitErr, stderrBuf.Bytes(), stdoutBuf.Bytes(), "extract")
	}

	if runErr != nil {
		return "", errors.Join(runErr, removeStaging(tmp))
	}

	return tmp, nil
}

// removeStaging discards a staging directory; the caller joins the result
// onto the verdict it is already reporting, so a failed cleanup is
// recorded litter, never a changed verdict.
func removeStaging(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("jobs: remove extract staging dir %q: %w", dir, err)
	}

	return nil
}

// staleStagingAge is the liveness bound for a staging dir: far past the
// 30-minute extraction wall clock, so no live run — this task's or a
// sibling's — can be mistaken for an orphan.
const staleStagingAge = 24 * time.Hour

// sweepStaleStaging removes .dl-tool-extract-* directories in root older
// than staleStagingAge — orphans of crashed or killed runs. A sweep
// failure is logged, never reported: leftover litter must not block the
// extraction it shares a root with.
func sweepStaleStaging(root string, maxAge time.Duration) {
	stale, err := filepath.Glob(filepath.Join(root, ".dl-tool-extract-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, dir := range stale {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("jobs: sweep of stale extract staging dir failed", "dir", dir, "err", err)
		}
	}
}

// pollProgress samples the bytes staged under tmp once per interval,
// feeds the task's 0–99 gauge and cancels the run the moment the written
// total crosses the cap — the declared headers are a pre-filter, not the
// enforcement. runCtx bounds the loop; writes use the caller's ctx, so a
// tick racing the run's end cannot fail on a canceled extract context.
// The poll never reports 100; the handler writes that itself once the
// tree is in place.
func (h *ExtractHandler) pollProgress(
	ctx, runCtx context.Context,
	taskID, dir string,
	declaredBytes, byteCap int64,
	cancel context.CancelFunc,
) chan error {
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(extractPollInterval)
		defer ticker.Stop()
		defer close(done)

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}

			written, err := dirSizeBytes(dir)
			if err != nil {
				continue // a mid-rename sample is noise, not a verdict
			}
			if written > byteCap {
				cancel()
				done <- fmt.Errorf("%w: %d bytes written exceed the %d cap", ErrCapExceeded, written, byteCap)

				return
			}
			if declaredBytes > 0 {
				percent := int(written * 100 / declaredBytes)
				if percent > 99 {
					percent = 99
				}
				if err := h.tasks.SetExtractProgress(ctx, taskID, percent); err != nil {
					cancel()
					done <- fmt.Errorf("jobs: extract task %q: %w", taskID, err)

					return
				}
			}
		}
	}()

	return done
}

// soleInnerArchive returns tmp's only entry when it is a regular file that
// is itself a supported archive — the inner container a gzip-wrapped
// payload needs a second pass for. Any other shape means the gzip held a
// plain payload and there is nothing to unwrap.
func soleInnerArchive(tmp string) string {
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 1 {
		return ""
	}
	entry := entries[0]
	if !entry.Type().IsRegular() || archiveExt(entry.Name()) == "" {
		return ""
	}

	return filepath.Join(tmp, entry.Name())
}

// verifyAndMove is pass 3: lstat every staged entry, refuse anything that
// is not a plain file or directory — a second hardlink or a link that
// slipped past pass 1 dies here — force the documented modes, recount the
// members and rename the tree into place. tmp and target share the
// filesystem, so the rename is atomic and a failure leaves target
// untouched.
func verifyAndMove(tmp, target string, caps Caps) error {
	count := 0
	err := filepath.WalkDir(tmp, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == tmp {
			return nil
		}
		count++
		if count > caps.MemberCount {
			return fmt.Errorf("%w: %d written members exceed the %d cap", ErrCapExceeded, count, caps.MemberCount)
		}

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := os.Chmod(path, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
				return fmt.Errorf("%w: %q is a hardlink", ErrInvalidArchive, path)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %q is not a regular file or directory", ErrInvalidArchive, path)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("jobs: verify extracted tree: %w", err)
	}

	// The staging root itself is tool-created 0o700 until verification
	// clears; the delivered tree keeps the mode the rename preserves, so
	// open it to the same 0o755 as every archive-delivered directory.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("jobs: open delivered extraction root: %w", err)
	}

	if err := os.Rename(tmp, target); err != nil {
		// A re-run after the first success — the worker reschedules a job
		// row stranded in running, or a duplicated enqueue claims twice —
		// finds its own output already in place. That is only the verdict
		// on a genuine name collision: the existing directory must match
		// the staged tree exactly, or this is a different payload's home
		// (a same-stem sibling archive) and the run must fail loudly.
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			if same, cmpErr := sameExtractedTree(tmp, target); cmpErr == nil && same {
				return removeStaging(tmp)
			}
		}

		return fmt.Errorf("jobs: move extracted tree into place: %w", err)
	}

	return nil
}

// sameExtractedTree reports whether dir already holds exactly the staged
// tree — same relative paths in both directions, same entry kinds, same
// file sizes — which is how an idempotent re-run recognises its own
// earlier output. A superset target or a type-swapped entry is a
// collision, not a re-run.
func sameExtractedTree(staged, dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false, err
	}

	if same, err := containedInTree(staged, dir); err != nil || !same {
		return same, err
	}

	return containedInTree(dir, staged)
}

// containedInTree reports whether every entry under src exists at the
// same relative path under dst with the same entry kind and, for regular
// files, the same size.
func containedInTree(src, dst string) (bool, error) {
	same := true
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		srcInfo, err := d.Info()
		if err != nil {
			return err
		}
		dstInfo, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil ||
			dstInfo.Mode().Type() != srcInfo.Mode().Type() ||
			(dstInfo.Mode().IsRegular() && srcInfo.Mode().IsRegular() && dstInfo.Size() != srcInfo.Size()) {
			same = false
			return filepath.SkipAll
		}

		return nil
	})

	return same, err
}

// dirSizeBytes sums the sizes of every regular file under dir — the
// bytes-written measurement the pass-2 cap and the progress gauge share.
func dirSizeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total, err
}

func sumMemberBytes(members []Member) int64 {
	var total int64
	for _, m := range members {
		total += m.Size
	}

	return total
}

// classifySevenzipError maps a failed 7zz run onto the error the task
// records. Signatures are matched anchored — a member named
// "No space.mkv" echoed in the listing must not steer a genuine crash
// into the disk-full bucket — so a verdict counts only at a line start
// or inside 7zz's own "ERROR:" marker.
func classifySevenzipError(err error, stderr, stdout []byte, op string) error {
	stderrText := string(stderr)
	stdoutText := string(stdout)
	detail := firstLine(stderrText + "\n" + stdoutText)
	switch {
	case diagnosticMatch(stderrText, "wrong password"),
		diagnosticMatch(stderrText, "cannot open encrypted archive"),
		diagnosticMatch(stdoutText, "wrong password"),
		diagnosticMatch(stdoutText, "cannot open encrypted archive"):
		return fmt.Errorf("%w: %s", ErrWrongPassword, detail)
	case diagnosticMatch(stderrText, "no space"), diagnosticMatch(stdoutText, "no space"):
		return fmt.Errorf("jobs: 7zz %s ran out of space: %w", op, fsx.ErrDiskFull)
	case diagnosticMatch(stderrText, "unexpected end of archive"),
		diagnosticMatch(stderrText, "is not archive"),
		diagnosticMatch(stderrText, "cannot open the file as archive"),
		diagnosticMatch(stderrText, "headers error"),
		diagnosticMatch(stdoutText, "unexpected end of archive"),
		diagnosticMatch(stdoutText, "is not archive"),
		diagnosticMatch(stdoutText, "cannot open the file as archive"),
		diagnosticMatch(stdoutText, "headers error"):
		return fmt.Errorf("%w: %s", ErrInvalidArchive, detail)
	}

	return fmt.Errorf("jobs: 7zz %s failed: %w: %s", op, err, detail)
}

// diagnosticMatch reports whether an output line either begins with the
// signature or carries it inside 7zz's "ERROR:" marker — the two shapes
// real diagnostics take. A member name embedded in a listing line can
// satisfy neither.
func diagnosticMatch(text, signature string) bool {
	for line := range strings.Lines(text) {
		line = strings.ToLower(strings.TrimSpace(line))
		if strings.HasPrefix(line, signature) {
			return true
		}
		if strings.HasPrefix(line, "error:") && strings.Contains(line, signature) {
			return true
		}
	}

	return false
}

// firstLine keeps one diagnostic line for the error message — the full
// dump can carry member names and stays out of the log and the database.
// Lines starting with 7zz's diagnostic prefixes win over the banner and
// member records; a member named "error.txt" must not pose as the
// verdict, so the match is anchored at the line start.
func firstLine(text string) string {
	fallback := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "7-Zip") {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "error") || strings.HasPrefix(lower, "cannot") ||
			strings.HasPrefix(lower, "unexpected") || strings.Contains(lower, "wrong password") {
			return line
		}
	}

	return fallback
}

// extractErrorCode maps a run failure onto the doc 04 section 4.2 code the
// task row records.
func extractErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrWrongPassword):
		return codeExtractFailedWrongPassword
	case errors.Is(err, ErrInvalidArchive):
		return codeExtractFailedInvalid
	case errors.Is(err, ErrCapExceeded):
		return codeExtractFailedQuota
	case fsx.IsENOSPC(err), errors.Is(err, fsx.ErrDiskFull):
		return codeExtractFailedDiskFull
	default:
		return codeExtractFailed
	}
}

// fail records the failure on the task — state error, the mapped
// error_code, the postprocess.extract.failed event inside the transition —
// then reports the job done: the verdict is durable, so the worker must
// not retry it. A task the operator moved out of extracting mid-run (a
// pause is what turns SetExtractProgress no-op) has already been answered
// elsewhere — there is nothing left to record.
func (h *ExtractHandler) fail(ctx context.Context, taskID string, runErr error) error {
	task, err := h.tasks.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobs: extract task %q: %w", taskID, err)
	}
	if task.State != "extracting" {
		return nil
	}

	code := extractErrorCode(runErr)

	// SetErrorCode is an idempotent overwrite, so a retry after a partial
	// first attempt converges; Transition is the commit point — if it
	// fails, the state is unchanged and the retry starts clean. An
	// error→error rejection means the first attempt's verdict already
	// landed and this attempt's writes are settled either way.
	codeErr := h.tasks.SetErrorCode(ctx, taskID, code, runErr.Error())
	transitionErr := h.tasks.Transition(ctx, taskID, "error", eventExtractFailed, runErr.Error())
	if errors.Is(transitionErr, store.ErrIllegalTransition) {
		transitionErr = nil
	}
	if err := errors.Join(transitionErr, codeErr); err != nil {
		// The verdict itself could not be recorded — this is the failure
		// the worker should retry.
		return fmt.Errorf("jobs: record extract failure of task %q: %w", taskID, err)
	}

	return nil
}

// cappedBuffer is the bounded sink for 7zz's combined output: the
// classifier needs the verdict lines, not an unbounded dump of member
// names a hostile archive could produce.
const sevenzipOutputCap = 64 << 10

type cappedBuffer struct {
	buf bytes.Buffer
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := sevenzipOutputCap - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}

	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}
