package uri

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// ErrNotTorrent is returned when the bytes are not a bencoded metainfo file.
var ErrNotTorrent = errors.New("uri: not a torrent file")

const (
	// maxTorrentBytes is the decoded .torrent size cap of doc 06 section 3.4.
	maxTorrentBytes = 10 << 20 // 10 MiB

	// maxTorrentFiles caps a manifest's file count.
	maxTorrentFiles = 100_000

	// maxBencodeDepth caps bencode nesting depth.
	maxBencodeDepth = 16

	// maxSegmentBytes is the per-segment byte budget of doc 12 section 3.2 step 8.
	maxSegmentBytes = 240

	// extensionWindow is how far back a '.' may sit and still count as the
	// extension (doc 12 section 3.2 step 8: a '.' within the last 9 characters).
	extensionWindow = 9

	// fileTreePropertiesKey is the "" key a BEP 52 file-tree leaf uses for its
	// {length, pieces root} properties (BEP 52).
	fileTreePropertiesKey = ""
)

// Manifest is the inspect-before-commit result for one submission. Producing it never touches disk and
// never creates an engine task.
type Manifest struct {
	Name       string
	TotalSize  int64
	Files      []ManifestFile
	InfohashV1 string // 40 lowercase hex, "" when the torrent has no v1 hash
	InfohashV2 string // 64 lowercase hex, "" when the torrent has no v2 hash
	Private    *bool  // nil when unknown
}

// ManifestFile is one file of a Manifest.
type ManifestFile struct {
	Index int
	Path  string // relative, cleaned; never absolute, never containing ".."
	Size  int64
}

// InspectTorrent parses raw .torrent bytes and computes both infohashes. It must not touch disk and
// must not contact any engine.
func InspectTorrent(b []byte) (Manifest, error) {
	infoBytes, err := infoDictBytes(b)
	if err != nil {
		return Manifest{}, err
	}

	var info metainfo.Info
	if err := bencode.Unmarshal(infoBytes, &info); err != nil {
		return Manifest{}, fmt.Errorf("%w: info dictionary: %v", ErrNotTorrent, err)
	}

	return manifestFromInfo(infoBytes, info)
}

// infoDictBytes returns the raw bencoded bytes of the top-level "info" value exactly as they appear in
// b. The hashes are computed over these bytes: re-encoding is forbidden, because unknown keys and
// non-canonical ordering in the wild make a re-encode differ from the original.
func infoDictBytes(b []byte) ([]byte, error) {
	if len(b) > maxTorrentBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte cap", ErrNotTorrent, len(b), maxTorrentBytes)
	}
	if len(b) == 0 || b[0] != 'd' {
		return nil, fmt.Errorf("%w: metainfo must be a top-level dictionary", ErrNotTorrent)
	}

	// Walk the top-level dictionary only; the info value's internals are
	// decoded separately, from the returned span.
	s := &bencodeScanner{b: b}
	s.pos++ // consume the opening 'd'
	for {
		if s.pos >= len(b) {
			return nil, fmt.Errorf("%w: top-level dictionary is unterminated", ErrNotTorrent)
		}
		if b[s.pos] == 'e' {
			return nil, fmt.Errorf("%w: no info key", ErrNotTorrent)
		}

		keyStart, keyEnd, err := s.parseString()
		if err != nil {
			return nil, err
		}
		valStart, valEnd, err := s.parseValue()
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(string(b[keyStart:keyEnd]), "info") {
			return b[valStart:valEnd], nil
		}
	}
}

// bencodeScanner walks bencode values tracking byte offsets and nesting
// depth. It exists so the info value can be hashed without re-encoding it;
// structural decoding is delegated to the metainfo module.
type bencodeScanner struct {
	b   []byte
	pos int
	// depth counts open containers; entered values are rejected past
	// maxBencodeDepth.
	depth int
}

func (s *bencodeScanner) errf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotTorrent, fmt.Sprintf(format, args...))
}

// parseValue advances past one bencode value and returns its [start, end)
// span, terminator included.
func (s *bencodeScanner) parseValue() (int, int, error) {
	if s.pos >= len(s.b) {
		return 0, 0, s.errf("unexpected end of input")
	}

	start := s.pos
	switch c := s.b[s.pos]; {
	case c == 'i':
		if err := s.parseInt(); err != nil {
			return 0, 0, err
		}
	case c == 'l' || c == 'd':
		if err := s.parseContainer(c); err != nil {
			return 0, 0, err
		}
	case c >= '0' && c <= '9':
		if err := s.parseStringSpan(); err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, s.errf("unexpected byte %q at offset %d", c, s.pos)
	}

	return start, s.pos, nil
}

// parseString parses a length-prefixed string and returns its content span.
func (s *bencodeScanner) parseString() (int, int, error) {
	if s.pos >= len(s.b) || s.b[s.pos] < '0' || s.b[s.pos] > '9' {
		return 0, 0, s.errf("expected string at offset %d", s.pos)
	}

	digitsStart := s.pos
	for s.pos < len(s.b) && s.b[s.pos] != ':' {
		if s.b[s.pos] < '0' || s.b[s.pos] > '9' {
			return 0, 0, s.errf("invalid string length at offset %d", digitsStart)
		}
		s.pos++
	}
	if s.pos >= len(s.b) {
		return 0, 0, s.errf("unterminated string length")
	}

	length, err := strconv.Atoi(string(s.b[digitsStart:s.pos]))
	if err != nil {
		return 0, 0, s.errf("string length at offset %d: %v", digitsStart, err)
	}
	s.pos++ // consume ':'
	if length > len(s.b)-s.pos {
		return 0, 0, s.errf("string of %d bytes overruns the input", length)
	}

	contentStart := s.pos
	s.pos += length

	return contentStart, s.pos, nil
}

// parseStringSpan is parseString for contexts that only need the cursor advanced.
func (s *bencodeScanner) parseStringSpan() error {
	_, _, err := s.parseString()

	return err
}

// parseInt consumes i<digits>e.
func (s *bencodeScanner) parseInt() error {
	s.pos++ // consume 'i'
	digitsStart := s.pos
	for s.pos < len(s.b) && s.b[s.pos] != 'e' {
		s.pos++
	}
	if s.pos >= len(s.b) {
		return s.errf("unterminated integer")
	}
	if _, err := strconv.ParseInt(string(s.b[digitsStart:s.pos]), 10, 64); err != nil {
		return s.errf("invalid integer at offset %d", digitsStart)
	}
	s.pos++ // consume 'e'

	return nil
}

// parseContainer consumes a list ('l') or dictionary ('d') to its terminator,
// enforcing the depth cap. Dictionary keys must be strings (BEP 3).
func (s *bencodeScanner) parseContainer(kind byte) error {
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > maxBencodeDepth {
		return s.errf("nesting depth exceeds %d", maxBencodeDepth)
	}

	s.pos++ // consume the marker
	for {
		if s.pos >= len(s.b) {
			return s.errf("unterminated %c at offset %d", kind, s.pos)
		}
		if s.b[s.pos] == 'e' {
			s.pos++

			return nil
		}
		if kind == 'd' {
			if s.b[s.pos] < '0' || s.b[s.pos] > '9' {
				return s.errf("dictionary key is not a string at offset %d", s.pos)
			}
			// A dictionary entry is a key-value pair: consume the key first.
			if _, _, err := s.parseString(); err != nil {
				return err
			}
		}
		if _, _, err := s.parseValue(); err != nil {
			return err
		}
	}
}

// manifestFromInfo builds the Manifest from the decoded info dict and its raw
// bytes. Hash rules are exactly the table of doc 06 section 3.5: v1-only hashes
// SHA-1, v2-only hashes SHA-256, a hybrid (meta version 2 plus pieces) hashes both.
func manifestFromInfo(infoBytes []byte, info metainfo.Info) (Manifest, error) {
	m := Manifest{Name: info.BestName(), Private: info.Private}

	// "meta version" = 2 and a `pieces` key together make a hybrid; per BEP 52
	// the v1 identity only exists when pieces are present.
	isV2 := info.MetaVersion == 2
	switch {
	case isV2 && len(info.Pieces) > 0:
		m.InfohashV1 = hex.EncodeToString(sha1Sum(infoBytes))
		m.InfohashV2 = hex.EncodeToString(sha256Sum(infoBytes))
	case isV2:
		m.InfohashV2 = hex.EncodeToString(sha256Sum(infoBytes))
	default:
		m.InfohashV1 = hex.EncodeToString(sha1Sum(infoBytes))
	}

	files, err := manifestFiles(info)
	if err != nil {
		return Manifest{}, err
	}
	m.Files = files
	for _, f := range files {
		m.TotalSize += f.Size
	}
	return m, nil
}

// manifestFiles builds the file list from the v1 fields when present, else the
// v2 file tree.
func manifestFiles(info metainfo.Info) ([]ManifestFile, error) {
	switch {
	case len(info.Files) > 0:
		return v1FileList(info)
	case info.MetaVersion == 2:
		return v2FileTree(info.FileTree)
	default:
		// Single-file v1: the torrent's name is the file.
		return manifestFileList([]ManifestFile{{Path: sanitiseSegment(info.BestName()), Size: info.Length}})
	}
}

// v1FileList maps the `files` list to manifest entries, joining path segments.
func v1FileList(info metainfo.Info) ([]ManifestFile, error) {
	files := make([]ManifestFile, 0, len(info.Files))
	for _, fi := range info.Files {
		path, err := joinSanitised(fi.Path)
		if err != nil {
			return nil, err
		}
		files = append(files, ManifestFile{Path: path, Size: fi.Length})
	}

	return manifestFileList(files)
}

// v2FileTree walks a BEP 52 file tree depth-first in key order.
func v2FileTree(tree metainfo.FileTree) ([]ManifestFile, error) {
	files, err := walkFileTree(tree, nil)
	if err != nil {
		return nil, err
	}

	return manifestFileList(files)
}

// walkFileTree collects the leaves of one subtree in sorted key order.
func walkFileTree(tree metainfo.FileTree, prefix []string) ([]ManifestFile, error) {
	files := []ManifestFile{}
	for _, name := range sortedTreeKeys(tree) {
		if name == fileTreePropertiesKey {
			continue
		}
		sub := tree.Dir[name]
		if !sub.IsDir() {
			path, err := joinSanitised(append(slices.Clone(prefix), name))
			if err != nil {
				return nil, err
			}
			files = append(files, ManifestFile{Path: path, Size: sub.File.Length})

			continue
		}
		subFiles, err := walkFileTree(sub, append(slices.Clone(prefix), name))
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}

	return files, nil
}

// sortedTreeKeys returns the subtree's keys, properties key first as BEP 52
// orders it, then the sorted member names.
func sortedTreeKeys(tree metainfo.FileTree) []string {
	keys := make([]string, 0, len(tree.Dir)+1)
	if _, ok := tree.Dir[fileTreePropertiesKey]; ok {
		keys = append(keys, fileTreePropertiesKey)
	}
	for name := range tree.Dir {
		if name != fileTreePropertiesKey {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)

	return keys
}

// manifestFileList assigns indices and enforces the file cap.
func manifestFileList(files []ManifestFile) ([]ManifestFile, error) {
	if len(files) > maxTorrentFiles {
		return nil, fmt.Errorf("%w: %d files exceeds the %d cap", ErrNotTorrent, len(files), maxTorrentFiles)
	}

	for i := range files {
		files[i].Index = i
	}

	return files, nil
}

// joinSanitised builds one relative path from raw segments. A raw ".."
// segment is hostile, not sloppy (doc 12 section 3.3): the whole entry is
// rejected rather than silently flattened.
func joinSanitised(segments []string) (string, error) {
	if len(segments) == 0 {
		return "", fmt.Errorf("%w: file entry has no path segments", ErrNotTorrent)
	}

	cleaned := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg == ".." || strings.HasPrefix(seg, "/") {
			return "", fmt.Errorf("%w: path segment %q escapes the torrent root", ErrNotTorrent, seg)
		}
		cleaned = append(cleaned, sanitiseSegment(seg))
	}

	return strings.Join(cleaned, "/"), nil
}

// reservedStems are the Windows device names of doc 12 section 3.2 step 10.
var reservedStems = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CLOCK$": true,
	"CONIN$": true, "CONOUT$": true,
}

// sanitiseSegment applies the doc 12 section 3.2 steps to one path component.
// NFC normalisation (step 3) needs golang.org/x/text and is owned by the
// filesystem task (T046, fsx.SanitiseSegment); every other step — the bidi
// and control deletions, the Windows-illegal set, UTF-8 repair, the 240-byte
// cap with extension preservation, the reserved names and the trailing-dot
// repair — is applied here, because manifest paths are shown and selected in
// the UI before T046's join runs.
func sanitiseSegment(s string) string {
	if s == "" {
		return "_"
	}

	s = stripBidiAndControls(s)
	s = replaceWindowsIllegal(s)
	s = strings.ToValidUTF8(s, "_")
	s = truncateWithExtension(s)
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

// stripBidiAndControls deletes the bidi and format codepoints of doc 12
// section 3.2 step 4 and every control character (step 5).
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

// replaceWindowsIllegal maps each of / \ : * ? " < > | onto _ (step 6).
func replaceWindowsIllegal(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\:*?"<>|/`, r) {
			return '_'
		}

		return r
	}, s)
}

// truncateWithExtension caps the segment at 240 bytes without splitting a
// codepoint, re-appending the extension the original carried — a '.' within
// the last 9 characters plus what follows it (step 8).
func truncateWithExtension(s string) string {
	if len(s) <= maxSegmentBytes {
		return s
	}

	stem, ext := splitExtension(s)
	budget := maxSegmentBytes - len(ext)
	if budget < 0 {
		budget = 0
	}

	return truncateRunes(stem, budget) + ext
}

// splitExtension returns the stem and the extension — a '.' within the last
// 9 characters plus everything after it.
func splitExtension(s string) (stem, ext string) {
	if len(s) > extensionWindow {
		if idx := strings.LastIndex(s[len(s)-extensionWindow:], "."); idx >= 0 {
			idx += len(s) - extensionWindow

			return s[:idx], s[idx:]
		}
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

// isReservedStem reports whether upper(stem) is a Windows device name (step 10).
func isReservedStem(s string) bool {
	stem, _ := splitExtension(s)

	return reservedStems[strings.ToUpper(stem)]
}

func sha1Sum(b []byte) []byte {
	sum := sha1.Sum(b)

	return sum[:]
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)

	return h[:]
}
