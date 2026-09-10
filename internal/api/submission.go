// The multipart submission form shared by POST /tasks and POST /tasks/inspect
// (docs/05-api-contract.md sections 5.2 and 5.3): one JSON "payload" part
// carrying the endpoint's body without blob, plus "file" parts that become
// torrent, metalink or URI-list submissions. The caps, the sniffs and the
// create-time destination and selection rules all live here.
package api

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/anacrolix/torrent/bencode"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/L-K-M/dl-tool/internal/fsx"
)

// Submission caps, from doc 05 section 5.2. Exceeding any of them is 413
// /problems/payload-too-large, except the URI count, which is 422
// /problems/validation-failed.
const (
	MaxRequestBytes = 32 << 20 // whole multipart request
	MaxBlobBytes    = 10 << 20 // one decoded .torrent or .metalink
	MaxURIs         = 50       // uris[] plus every line of every .txt part
)

// The two part names the form defines; anything else is refused, never
// guessed into a submission.
const (
	payloadPart = "payload"
	filePart    = "file"
)

// The kinds classifyUpload decides between, the strings the part-handling
// table of the task uses.
const (
	uploadKindTorrent  = "torrent"
	uploadKindMetalink = "metalink"
	uploadKindText     = "text"
)

var (
	// ErrPayloadTooLarge marks the 413 breaches of MaxRequestBytes and
	// MaxBlobBytes.
	ErrPayloadTooLarge = errors.New("api: submission exceeds a size cap")

	// ErrMalformedForm marks a multipart form that could not be read at all:
	// a bad boundary, a truncated body, a second payload part.
	ErrMalformedForm = errors.New("api: malformed multipart form")

	// ErrUnsupportedUpload marks a file part whose bytes are neither a
	// torrent, a metalink nor UTF-8 text. The endpoint answers with one
	// rejected[] entry, never a guess.
	ErrUnsupportedUpload = errors.New("api: unsupported file part")

	// ErrInvalidSelection marks a select_files entry that does not resolve
	// against the manifest; the create endpoint answers 422 with it.
	ErrInvalidSelection = errors.New("api: invalid file selection")
)

// UploadedFile is one "file" part of the multipart form.
type UploadedFile struct {
	Name  string // as sent by the client, used only for the display name
	Bytes []byte
}

// uploadedFilesKey carries the parsed file parts from the form middleware to
// the huma handler, which receives only the request context.
type uploadedFilesKey struct{}

// acceptSubmissionForm is the operation middleware that lets a submission
// operation take multipart/form-data beside application/json: it parses the
// form with the caps of this file, exposes the file parts on the request
// context, and swaps the request body and content type for the payload part
// so Huma's ordinary JSON path parses exactly what a JSON client would have
// sent. A form that breaches a cap is answered here, before any handler or
// store runs.
func acceptSubmissionForm(ctx huma.Context, next func(huma.Context)) {
	mediaType, _, err := mime.ParseMediaType(ctx.Header("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		next(ctx)

		return
	}

	// humachi is the only adapter this server is built on. Unwrap yields the
	// live request and writer: the swap below rewrites the same *http.Request
	// the inner handler reads, and the writer answers the capped case.
	r, w := humachi.Unwrap(ctx)

	payload, files, err := parseSubmission(r)
	if err != nil {
		writeProblem(w, submissionFormProblem(err))

		return
	}

	if len(payload) == 0 {
		// An absent or empty payload part is an empty JSON body, not an error
		// (the contract of parseSubmission): a parts-only submission proceeds
		// on a zero-valued body.
		payload = []byte("{}")
	}

	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(bytes.NewReader(payload))
	r.ContentLength = int64(len(payload))

	next(huma.WithValue(ctx, uploadedFilesKey{}, files))
}

// uploadedFilesFrom returns the file parts the form middleware stashed, or
// nil on an ordinary JSON request.
func uploadedFilesFrom(ctx context.Context) []UploadedFile {
	files, _ := ctx.Value(uploadedFilesKey{}).([]UploadedFile)

	return files
}

// submissionFormProblem maps the parse sentinels onto the registered
// problem shapes. The detail is fixed text: a parse error can echo attacker
// bytes, so the sentinel's message never reaches the wire.
func submissionFormProblem(err error) error {
	switch {
	case errors.Is(err, ErrPayloadTooLarge):
		return Problem(
			SlugPayloadTooLarge,
			http.StatusRequestEntityTooLarge,
			"the multipart submission exceeds its size cap: 33554432 bytes per request, 10485760 per file part",
		)
	case errors.Is(err, ErrMalformedForm):
		return Problem(
			SlugValidationFailed,
			http.StatusBadRequest,
			"the multipart form is malformed: exactly one payload part and zero or more file parts are expected",
		)
	default:
		return internalProblem()
	}
}

// parseSubmission reads a multipart/form-data body: exactly one "payload"
// part holding the endpoint's JSON body without blob, and zero or more
// "file" parts. It returns ErrPayloadTooLarge above MaxRequestBytes and
// never buffers more than that.
func parseSubmission(r *http.Request) ([]byte, []UploadedFile, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, nil, fmt.Errorf("%w: the content type is not multipart/form-data with a boundary", ErrMalformedForm)
	}

	// The cap wraps the body before any read, so a request above it is cut at
	// the limit however large the client keeps writing. A nil writer is
	// supported: the server-side connection teardown it would trigger is the
	// transport's business, not the parser's.
	r.Body = http.MaxBytesReader(nil, r.Body, MaxRequestBytes)
	reader := multipart.NewReader(r.Body, params["boundary"])

	var payload []byte
	files := []UploadedFile{}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, formReadError(err)
		}

		// readFormPart consumes the part whole, so no Close is needed to
		// keep the reader positioned; an error returns at once.
		switch part.FormName() {
		case payloadPart:
			if payload != nil {
				return nil, nil, fmt.Errorf("%w: part %q appears more than once", ErrMalformedForm, payloadPart)
			}
			payload, err = readFormPart(part, MaxRequestBytes)
		case filePart:
			var content []byte
			content, err = readFormPart(part, MaxBlobBytes)
			if err == nil {
				files = append(files, UploadedFile{Name: part.FileName(), Bytes: content})
			}
		default:
			return nil, nil, fmt.Errorf(
				"%w: part %q is neither %q nor %q", ErrMalformedForm, part.FormName(), payloadPart, filePart,
			)
		}
		if err != nil {
			return nil, nil, formReadError(err)
		}
	}

	return payload, files, nil
}

// readFormPart reads one part whole, refusing it above cap. The payload
// part is capped by the request budget alone; a file part by the blob cap —
// enforced before classification, so an oversized .txt list is refused with
// the same 413: a URI list is bounded to 50 short lines by MaxURIs, and
// anything beyond the blob cap can only be a client error or an attack.
func readFormPart(part io.Reader, cap int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(part, cap+1))
	if err != nil {
		return nil, formReadError(err)
	}
	if int64(len(content)) > cap {
		return nil, fmt.Errorf("%w: a part exceeds the %d-byte cap", ErrPayloadTooLarge, cap)
	}

	return content, nil
}

// formReadError classifies a read failure: a cap breach stays the 413 it
// is (readFormPart's own or the MaxBytesReader's), anything else is a
// malformed form.
func formReadError(err error) error {
	if errors.Is(err, ErrPayloadTooLarge) {
		return err
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return fmt.Errorf("%w: the request exceeds the %d-byte cap", ErrPayloadTooLarge, MaxRequestBytes)
	}

	return fmt.Errorf("%w: %v", ErrMalformedForm, err)
}

// uploadBlob is one file part classified as a torrent or a metalink. The
// bytes are parsed for their manifest at plan time and then discarded —
// never written to disk (the task's out-of-scope rule).
type uploadBlob struct {
	name  string
	kind  string // uploadKindTorrent | uploadKindMetalink
	bytes []byte
}

// processUploads folds the file parts of a submission into URI lines and
// classified blobs, the shared front half of both submission endpoints. An
// unrecognised part is one rejected[] entry with
// /problems/unsupported-media-type, never a guess (the part-handling table
// of the task); the per-part size cap was already enforced at parse time.
func processUploads(files []UploadedFile) (uris []string, blobs []uploadBlob, rejected []RejectedURI) {
	rejected = []RejectedURI{}

	for _, f := range files {
		kind, err := classifyUpload(f)
		if err != nil {
			rejected = append(rejected, RejectedURI{
				URI:    f.Name,
				Type:   SlugUnsupportedMediaType,
				Detail: sentinelDetail(err, ErrUnsupportedUpload),
			})

			continue
		}
		if kind == uploadKindText {
			uris = append(uris, expandTextList(f.Bytes)...)

			continue
		}
		blobs = append(blobs, uploadBlob{name: f.Name, kind: kind, bytes: f.Bytes})
	}

	return uris, blobs, rejected
}

// expandTextList returns one entry per line of a .txt part, dropping empty
// lines and lines whose first non-space character is '#'. Order is preserved
// and duplicates are kept.
func expandTextList(b []byte) []string {
	entries := []string{}

	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}

	return entries
}

// classifyUpload decides what one part becomes: "torrent", "metalink" or
// "text", from the sniffed bytes first and the filename extension only as a
// tie-break. An unrecognised part is rejected, never guessed. The client
// Content-Type is never consulted: the bytes are a part's only honest
// identity.
func classifyUpload(f UploadedFile) (string, error) {
	// The tie the extension breaks: a .txt list whose first line happens to
	// parse as a bencoded dictionary or hold a metalink snippet is a text
	// list the user named .txt, not a torrent or a metalink.
	if !strings.EqualFold(filepath.Ext(f.Name), ".txt") {
		switch {
		case isBencodeDict(f.Bytes):
			return uploadKindTorrent, nil
		case isMetalinkXML(f.Bytes):
			return uploadKindMetalink, nil
		}
	}
	if utf8.Valid(f.Bytes) && !bytes.ContainsRune(f.Bytes, 0) {
		return uploadKindText, nil
	}

	return "", fmt.Errorf("%w: %q is neither a torrent, a metalink nor UTF-8 text", ErrUnsupportedUpload, f.Name)
}

// isBencodeDict reports whether the bytes are one complete bencoded
// dictionary — a torrent part's shape. The client's Content-Type is never
// consulted.
func isBencodeDict(b []byte) bool {
	if len(b) == 0 || b[0] != 'd' {
		return false
	}

	var decoded any
	if err := bencode.Unmarshal(b, &decoded); err != nil {
		return false
	}

	_, ok := decoded.(map[string]any)

	return ok
}

// isMetalinkXML reports whether the bytes are an XML document whose root
// element is metalink, prolog, comments and directives skipped. Text that is
// not XML at all fails the token walk and falls through to the text sniff.
func isMetalinkXML(b []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(b))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}

		switch t := token.(type) {
		case xml.ProcInst, xml.Directive, xml.Comment:
			continue
		case xml.CharData:
			// Only whitespace may precede the root element of an XML document.
			if strings.TrimSpace(string(t)) != "" {
				return false
			}
			continue
		case xml.StartElement:
			return t.Name.Local == "metalink"
		default:
			return false
		}
	}
}

// subfolderDestination returns filepath.Join(destination,
// sanitiseSegment(manifestName)) when createSubfolder is set and the caller
// has established a multi-file manifest, and destination otherwise. The
// result is re-checked with fsx.ResolveDestination before use.
func subfolderDestination(roots []string, destination, manifestName string, createSubfolder bool) (string, error) {
	if !createSubfolder {
		return destination, nil
	}

	// sanitiseSegment strips separators and traversal spellings, so the join
	// appends exactly one component; the resolve re-checks the result —
	// symlinks included — against the configured roots before any task row
	// carries it.
	return fsx.ResolveDestination(roots, filepath.Join(destination, sanitiseSegment(manifestName)))
}

// multipartSubmissionBody declares the multipart/form-data alternative of a
// submission operation for the generated document (task step 9). Huma's own
// registration adds the application/json media type beside it, and the
// request-time schema stays the JSON one: the middleware above rewrites the
// form into a JSON request before Huma's body pipeline runs.
func multipartSubmissionBody() *huma.RequestBody {
	return &huma.RequestBody{
		Content: map[string]*huma.MediaType{
			"multipart/form-data": {
				Schema: &huma.Schema{
					Type:        "object",
					Description: "One payload part holding the operation's JSON body without blob, plus zero or more file parts: a .torrent or .metalink becomes one task each, a .txt contributes every accepted line to uris.",
					Properties: map[string]*huma.Schema{
						payloadPart: {Type: "string", Description: "The operation's JSON body, without blob"},
						filePart: {
							Type:        "array",
							Items:       &huma.Schema{Type: "string", Format: "binary"},
							Description: ".torrent, .metalink or .txt uploads",
						},
					},
				},
			},
		},
	}
}

// FileSelectionRequest is one entry of the create body's select_files. It
// reuses the priority vocabulary of doc 06 section 1.1 and is applied to the
// first multi-file manifest of the submission.
type FileSelectionRequest struct {
	Index    int     `json:"index"    minimum:"0"`
	Selected *bool   `json:"selected,omitempty"`
	Priority *string `json:"priority,omitempty" enum:"skip,normal,high,maximum"`
}

// applySelection turns select_files into the AddRequest fields: the
// indices to download (AddRequest.SelectFiles) and the resolved priority of
// every addressed file, the same shape resolveFileSelection hands
// Engine.SetFiles. A negative fileCount means the manifest is unknown — a
// magnet whose metadata is pending, an unparsed metalink — so indices are
// taken on trust for the engine to judge at add time. It returns 422
// material when an index is outside the manifest; the per_file_select and
// per_file_priority capability checks are the caller's, the routed engine
// not being a parameter here.
func applySelection(sel []FileSelectionRequest, fileCount int) (indices []int, priorities map[int]int, err error) {
	indices = []int{}
	priorities = make(map[int]int, len(sel))
	seen := make(map[int]bool, len(sel))

	for _, entry := range sel {
		if entry.Index < 0 || (fileCount >= 0 && entry.Index >= fileCount) {
			return nil, nil, fmt.Errorf("%w: file index %d is outside the %d-file manifest", ErrInvalidSelection, entry.Index, fileCount)
		}
		if seen[entry.Index] {
			return nil, nil, fmt.Errorf("%w: file index %d appears more than once", ErrInvalidSelection, entry.Index)
		}
		seen[entry.Index] = true

		priority, selected, err := resolveSelectionEntry(entry)
		if err != nil {
			return nil, nil, err
		}
		priorities[entry.Index] = priority
		if selected {
			indices = append(indices, entry.Index)
		}
	}

	sort.Ints(indices)

	return indices, priorities, nil
}

// resolveSelectionEntry resolves one entry onto its priority integer and
// selected flag: a priority implies its selection, a selection without a
// priority is normal when true and skip when false, and an entry carrying
// both must agree — selected:false and priority:"high" are two answers to
// one question. The vocabulary and its integers are shared with the files
// endpoints of doc 05 section 5.8.
func resolveSelectionEntry(entry FileSelectionRequest) (int, bool, error) {
	if entry.Selected == nil && entry.Priority == nil {
		return 0, false, fmt.Errorf("%w: index %d needs at least one of selected and priority", ErrInvalidSelection, entry.Index)
	}

	priority := priorityNormal
	if entry.Selected != nil && !*entry.Selected {
		priority = prioritySkip
	}
	selected := entry.Selected == nil || *entry.Selected

	if entry.Priority != nil {
		value, ok := priorityValues[*entry.Priority]
		if !ok {
			return 0, false, fmt.Errorf("%w: index %d carries unknown priority %q", ErrInvalidSelection, entry.Index, *entry.Priority)
		}
		if entry.Selected != nil && *entry.Selected != (value != prioritySkip) {
			return 0, false, fmt.Errorf("%w: index %d disagrees; selected and priority are one concept", ErrInvalidSelection, entry.Index)
		}
		priority = value
		selected = value != prioritySkip
	}

	return priority, selected, nil
}

// sentinelDetail strips the sentinel prefix, the same cut rejectURI makes
// of uri.ErrUnsupportedScheme: the wire carries the reason alone.
func sentinelDetail(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}

// sanitiseSegment applies the doc 12 section 3.2 steps to one path
// component — the manifest-name half of create_subfolder. It mirrors
// internal/uri's unexported segment sanitiser for manifest paths, which this
// package cannot import and whose file this task may not touch; T046 lifts
// both into fsx.SanitiseSegment with the NFC normalisation x/text adds. The
// two read the same except step 10, which here stems on the first dot (see
// isReservedStem): until T046 unifies them, a multi-dot reserved name
// sanitises differently in the two copies ("nul.tar.gz" → "_nul.tar.gz"
// here, "nul.tar.gz" there). The two never pair — this copy sees only the
// manifest name, the uri copy only file-path segments inside InspectTorrent
// — and no code may couple their outputs until the unification.
func sanitiseSegment(s string) string {
	if s == "" {
		return "_"
	}

	s = stripBidiAndControls(s)
	s = replaceWindowsIllegal(s)
	s = strings.ToValidUTF8(s, "_")
	s = truncateSegmentWithExtension(s)
	s = strings.TrimRight(s, ". ")
	s = strings.TrimSpace(s)
	if isReservedStem(s) {
		s = "_" + s
	}
	if s == "" || s == "." || s == ".." {
		return "_"
	}

	return s
}

// segmentMaxBytes is the 240-byte cap of doc 12 section 3.2 step 8.
const segmentMaxBytes = 240

// extensionWindow is how far back a '.' may sit and still count as the
// extension (step 8: a '.' within the last 9 characters plus what follows).
const extensionWindow = 9

// stripBidiAndControls deletes the bidi and format codepoints of step 4 and
// every control character (step 5).
func stripBidiAndControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E,
			r >= 0x2066 && r <= 0x2069, r == 0xFEFF:
			return -1
		case r < 0x20 || r == 0x7F:
			return -1
		}

		return r
	}, s)
}

// replaceWindowsIllegal maps each of / \ : * ? " < > | onto _ (step 6):
// /data is routinely re-exported over SMB.
func replaceWindowsIllegal(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\:*?"<>|/`, r) {
			return '_'
		}

		return r
	}, s)
}

// truncateSegmentWithExtension caps the segment at 240 bytes without
// splitting a codepoint, re-appending the extension the original carried.
func truncateSegmentWithExtension(s string) string {
	if len(s) <= segmentMaxBytes {
		return s
	}

	stem, ext := splitSegmentExtension(s)
	budget := segmentMaxBytes - len(ext)
	if budget < 0 {
		budget = 0
	}

	return truncateRunes(stem, budget) + ext
}

// splitSegmentExtension returns the stem and the extension — a '.' within
// the last 9 characters plus everything after it. Short names search their
// whole length so a short extension still re-appends after truncation.
// Only truncateSegmentWithExtension consumes this; the reserved-name check
// stems on the first dot instead (see isReservedStem).
func splitSegmentExtension(s string) (stem, ext string) {
	tail := s
	if len(s) > extensionWindow {
		tail = s[len(s)-extensionWindow:]
	}
	if idx := strings.LastIndex(tail, "."); idx >= 0 {
		idx += len(s) - len(tail)

		return s[:idx], s[idx:]
	}

	return s, ""
}

// truncateRunes cuts s to at most n bytes without splitting a codepoint.
func truncateRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}

// reservedStems are the Windows device names of step 10.
var reservedStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CLOCK$": true,
	"CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isReservedStem reports whether upper(stem) is a Windows device name.
// The candidate is everything before the FIRST '.' — a device name stays
// reserved with any number of extensions attached ("nul.txt",
// "com9.tar.gz") and however long they are ("con.abcdefghi"), while an
// exact match keeps "connect.txt" and "nulled.bin" untouched. The
// extension-window split of step 8 answers a different question (what to
// re-append after truncation) and must not feed this check. Diverges from
// internal/uri's window-derived copy, which T046 lifts into
// fsx.SanitiseSegment with the doc 12 3.4 verbatim table.
func isReservedStem(s string) bool {
	stem, _, _ := strings.Cut(s, ".")

	return reservedStems[strings.ToUpper(stem)]
}
