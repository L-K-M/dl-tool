#!/usr/bin/env python3
"""Regenerate the committed yt-dlp routing table (ADR-0022).

Reads YTDLP_VERSION and YTDLP_SHA256_WHEEL from the Dockerfile, downloads the
pinned py3-none-any wheel from PyPI, verifies its SHA-256 BEFORE unpacking or
importing anything, then transpiles every extractor's _VALID_URL to RE2 and
writes, deterministically:

  internal/engine/ytdlp/extractor_patterns.txt   one RE2 pattern per line,
                                                  generic never emitted
  internal/engine/ytdlp/extractors_residual.txt  extractor names whose patterns
                                                  could not be made RE2-safe;
                                                  each needs a ResidualOverrides
                                                  entry in overrides.go

Usage:

  scripts/gen-ytdlp-patterns.py [--yt-dlp PATH]

--yt-dlp PATH accepts an unpacked wheel directory (trusted, maintainer-supplied)
or a .whl file (still hash-checked against the Dockerfile pin). The imported
yt_dlp.version.__version__ must match YTDLP_VERSION either way.

Requires `go` on PATH. The RE2 compile probe runs inside the repository so
go.mod's toolchain directive (GOTOOLCHAIN default behaviour) selects which
regexp syntax compiles.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCKERFILE = os.path.join(REPO, "Dockerfile")
PATTERNS_OUT = os.path.join(REPO, "internal/engine/ytdlp/extractor_patterns.txt")
RESIDUAL_OUT = os.path.join(REPO, "internal/engine/ytdlp/extractors_residual.txt")

# The probe compiles one pattern per line and prints the 0-based index of every
# line regexp.Compile rejects. It lives inside the module for the run so the
# repo's go.mod — its toolchain directive included — governs the compiler.
PROBE_GO = """\
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for i, line := range strings.Split(string(data), "\\n") {
		if line == "" {
			continue
		}
		if _, err := regexp.Compile(line); err != nil {
			fmt.Println(i)
		}
	}
}
"""


def die(msg):
    sys.exit(f"gen-ytdlp-patterns: error: {msg}")


def dockerfile_arg(name):
    m = re.search(
        rf'^ARG\s+{re.escape(name)}\s*=\s*(?:"([^"]*)"|\'([^\']*)\'|(\S+))',
        open(DOCKERFILE, encoding="utf-8").read(),
        re.MULTILINE,
    )
    if not m:
        die(f"Dockerfile does not declare ARG {name}")
    return next(g for g in m.groups() if g is not None)


def normalized(version):
    """PyPI-normalize a version tag: 2026.08.19 -> 2026.8.19."""
    try:
        return ".".join(str(int(seg)) for seg in version.split("."))
    except ValueError:
        return version


def wheel_url(version):
    with urllib.request.urlopen(
        f"https://pypi.org/pypi/yt-dlp/{normalized(version)}/json", timeout=60
    ) as resp:
        meta = json.load(resp)
    for entry in meta["urls"]:
        if entry["filename"].endswith("-py3-none-any.whl"):
            return entry["url"], entry["filename"]
    die(f"PyPI release yt-dlp {normalized(version)} carries no py3-none-any wheel")


def fetch_wheel(version, want_sha256):
    url, filename = wheel_url(version)
    with urllib.request.urlopen(url, timeout=300) as resp:
        data = resp.read()
    got = hashlib.sha256(data).hexdigest()
    if got != want_sha256:
        die(
            f"{filename} sha256 mismatch: got {got}, Dockerfile pins "
            f"{want_sha256} — refusing to unpack"
        )
    return data, filename


def stage_package(wheel_bytes, workdir):
    """Unpack the hash-verified wheel and return the import root."""
    wheel_path = os.path.join(workdir, "yt_dlp.whl")
    with open(wheel_path, "wb") as f:
        f.write(wheel_bytes)
    pkg_root = os.path.join(workdir, "pkg")
    with zipfile.ZipFile(wheel_path) as zf:
        zf.extractall(pkg_root)
    return pkg_root


def resolve_source(path, want_sha256, workdir):
    """--yt-dlp: a wheel file still faces the hash gate; an unpacked directory
    is maintainer-supplied and trusted as-is."""
    if os.path.isdir(path):
        pkg_root = (
            path
            if os.path.isdir(os.path.join(path, "yt_dlp"))
            else os.path.dirname(path)
        )
        if not os.path.isdir(os.path.join(pkg_root, "yt_dlp")):
            die(f"--yt-dlp {path}: no yt_dlp package found there or one level up")
        return pkg_root
    if not os.path.isfile(path):
        die(f"--yt-dlp {path}: not a directory or a wheel file")
    data = open(path, "rb").read()
    got = hashlib.sha256(data).hexdigest()
    if got != want_sha256:
        die(
            f"{path} sha256 mismatch: got {got}, Dockerfile pins "
            f"{want_sha256} — refusing to unpack"
        )
    return stage_package(data, workdir)


def iter_patterns(pkg_root, want_version):
    sys.path.insert(0, pkg_root)
    import yt_dlp.version  # noqa: E402

    got = yt_dlp.version.__version__
    if normalized(got) != normalized(want_version):
        die(
            f"imported yt_dlp is version {got}, Dockerfile pins "
            f"{want_version} — refusing to generate"
        )

    from yt_dlp.extractor import gen_extractor_classes  # noqa: E402
    from yt_dlp.utils import variadic  # noqa: E402

    for cls in gen_extractor_classes():
        if cls.ie_key() == "Generic":
            continue
        valid = getattr(cls, "_VALID_URL", None)
        if valid is False or valid is None:
            continue
        for pattern in variadic(valid):
            if not isinstance(pattern, str):
                # Not a regex source (a compiled object, a callable): nothing
                # here can transpile it, so it must surface on the residual
                # list for an override entry rather than silently dropping.
                yield cls.ie_key(), None
                continue
            yield cls.ie_key(), pattern


def close_paren(s, i):
    """Index of the ')' matching the '(' at s[i]; -1 when unbalanced."""
    depth, in_class = 0, False
    j = i
    while j < len(s):
        c = s[j]
        if c == "\\":
            j += 2
            continue
        if in_class:
            if c == "]":
                in_class = False
        elif c == "[":
            in_class = True
        elif c == "(":
            depth += 1
        elif c == ")":
            depth -= 1
            if depth == 0:
                return j
        j += 1
    return -1


def strip_verbose(s):
    """Drop unescaped whitespace and #-to-EOL comments outside classes."""
    out = []
    in_class = in_comment = False
    i = 0
    while i < len(s):
        c = s[i]
        if in_comment:
            if c == "\n":
                in_comment = False
            i += 1
            continue
        if c == "\\":
            out.append(s[i : i + 2])
            i += 2
            continue
        if in_class:
            if c == "]":
                in_class = False
            out.append(c)
        elif c == "[":
            in_class = True
            out.append(c)
        elif c == "#":
            in_comment = True
        elif not c.isspace():
            out.append(c)
        i += 1
    return "".join(out)


def rewrite_captures(s):
    """Rewrite (…) and (?P<name>…) to (?:…): routing needs a boolean match,
    and named groups would collide inside the merged alternation."""
    out = []
    in_class = False
    i = 0
    while i < len(s):
        c = s[i]
        if c == "\\":
            out.append(s[i : i + 2])
            i += 2
            continue
        if in_class:
            if c == "]":
                in_class = False
            out.append(c)
            i += 1
            continue
        if c == "[":
            in_class = True
            out.append(c)
            i += 1
            continue
        if c == "(" and i + 1 < len(s) and s[i + 1] != "?":
            out.append("(?:")
            i += 1
            continue
        m = re.match(r"\(\?P<[^>]*>", s[i:]) if c == "(" else None
        if m:
            out.append("(?:")
            i += m.end()
            continue
        out.append(c)
        i += 1
    return "".join(out)


# Python's re reads the shorthands as Unicode classes for str patterns while
# RE2's are ASCII-only: \w is exactly \p{L}\p{N} plus the underscore (Python
# excludes marks and other connector punctuation), \d is \p{Nd}, and \s is the
# ASCII whitespace set plus \x1c-\x1f, \x85 and everything in \p{Z}. Widening
# is load-bearing for routing, not pedantry — the extractor's own _VALID_URL
# accepts URLs the ASCII transpile would miss (measured on the 2026.08.19
# corpus: PlayerFM's `[\w-]+` slugs match
# https://player.fm/series/ポッドキャスト/ep-1 under re.match but not under
# Go's regexp), and a table miss is a silent aria2 fall-through.
_WORD = r"\p{L}\p{N}_"
_SPACE = r"\t\n\f\r\v\x{1c}-\x{1f}\x{85}\p{Z}"


def widen_shorthands(s):
    """Widen \\d \\w \\s and their complements to Python's Unicode semantics.
    Returns the widened pattern, or None to mark it residual.

    \\b and \\B pass through: RE2 has no Unicode word boundary, and on this
    corpus every \\b sits beside an ASCII literal (`\\bid=`, `\\bv=` — query
    parameter names) where the two semantics agree; any residual divergence
    over-matches, which row 3 tolerates because yt-dlp still arbitrates.

    Inside a character class the affirmative shorthands flatten into the
    class body; a negated shorthand (\\D \\W \\S) cannot be expressed there —
    RE2 has no nested negated classes — unless the class is the complementary
    pair itself ([\\s\\S] and friends, the any-character idiom), which
    collapses to (?s:.). Anything else lands residual rather than guessing.
    """
    out = []
    i = 0
    while i < len(s):
        c = s[i]
        if c == "\\" and i + 1 < len(s):
            nxt = s[i + 1]
            if nxt == "d":
                out.append(r"\p{Nd}")
            elif nxt == "D":
                out.append(r"\P{Nd}")
            elif nxt == "w":
                out.append("[" + _WORD + "]")
            elif nxt == "W":
                out.append("[^" + _WORD + "]")
            elif nxt == "s":
                out.append("[" + _SPACE + "]")
            elif nxt == "S":
                out.append("[^" + _SPACE + "]")
            else:
                out.append(s[i : i + 2])
            i += 2
            continue
        if c != "[":
            out.append(c)
            i += 1
            continue
        # Character class: tokenize the body, keeping escapes whole.
        j = i + 1
        body = []
        while j < len(s):
            if s[j] == "\\" and j + 1 < len(s):
                body.append(s[j : j + 2])
                j += 2
                continue
            if s[j] == "]":
                break
            body.append(s[j])
            j += 1
        if j == len(s):
            return None  # unbalanced class
        if (
            len(body) == 2
            and body[0].startswith("\\")
            and body[1].startswith("\\")
            and {body[0][1], body[1][1]} in ({"d", "D"}, {"w", "W"}, {"s", "S"})
        ):
            out.append("(?s:.)")  # [x\X] is the match-anything idiom
            i = j + 1
            continue
        inner = []
        for tok in body:
            if tok == r"\d":
                inner.append(r"\p{Nd}")
            elif tok == r"\w":
                inner.append(_WORD)
            elif tok == r"\s":
                inner.append(_SPACE)
            elif tok in (r"\D", r"\W", r"\S"):
                return None
            else:
                inner.append(tok)
        out.append("[" + "".join(inner) + "]")
        i = j + 1
    return "".join(out)


def transpile(pattern):
    """Python re -> RE2. Returns the pattern, or None to mark it residual.

    The verbose flag is pattern-initial in every corpus case: strip x from a
    leading flag group while preserving co-flags, then drop whitespace and
    comments inside the verbose region only — the whole pattern for a bare
    (?x), the interior of a leading (?x:…) group that reaches end-of-pattern.
    Anything else lands residual rather than guessing.
    """
    if "(?x" not in pattern:
        return widen_shorthands(rewrite_captures(pattern))

    m = re.match(r"\(\?([a-zA-Z]+)(:|\))", pattern)
    if not m or "x" not in m.group(1):
        return None
    coflags = m.group(1).replace("x", "")

    if m.group(2) == ")":
        head = f"(?{coflags})" if coflags else ""
        return widen_shorthands(
            rewrite_captures(strip_verbose(head + pattern[m.end() :]))
        )

    close = close_paren(pattern, 0)
    if close != len(pattern) - 1:
        return None
    head = f"(?{coflags}:" if coflags else "(?:"
    return widen_shorthands(
        rewrite_captures(head + strip_verbose(pattern[m.end() : close]) + ")")
    )


def probe_compile(patterns):
    """Return the set of 0-based indices regexp.Compile rejects, under the
    repo's pinned toolchain."""
    probe_dir = tempfile.mkdtemp(prefix="ytdlp-probe-", dir=REPO)
    candidates = None
    try:
        with open(os.path.join(probe_dir, "main.go"), "w", encoding="utf-8") as f:
            f.write(PROBE_GO)
        with tempfile.NamedTemporaryFile(
            "w", suffix=".txt", delete=False, encoding="utf-8"
        ) as tf:
            candidates = tf.name
            tf.write("\n".join(patterns) + "\n")
        rel = "./" + os.path.relpath(probe_dir, REPO)
        out = subprocess.run(
            ["go", "run", rel, candidates],
            cwd=REPO,
            capture_output=True,
            text=True,
        )
        if out.returncode != 0:
            die(f"go compile probe failed: {out.stderr.strip()}")
        # Fail closed on unexpected output: the probe prints only 0-based
        # indices, so anything else means the pass/fail mapping is
        # untrustworthy and the run must not produce a table.
        failed = set()
        for line in out.stdout.split():
            try:
                failed.add(int(line))
            except ValueError:
                die(f"go compile probe emitted unexpected stdout: {line!r}")
        return failed
    finally:
        shutil.rmtree(probe_dir, ignore_errors=True)
        if candidates is not None:
            os.unlink(candidates)


def write_table(path, header, rows):
    with open(path, "w", encoding="utf-8") as f:
        f.write(header)
        for row in rows:
            f.write(row + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument(
        "--yt-dlp",
        metavar="PATH",
        help="unpacked wheel directory or .whl file to read instead of "
        "downloading from PyPI (a wheel still faces the SHA-256 gate)",
    )
    args = ap.parse_args()

    version = dockerfile_arg("YTDLP_VERSION")
    wheel_sha = dockerfile_arg("YTDLP_SHA256_WHEEL")

    with tempfile.TemporaryDirectory() as workdir:
        if args.yt_dlp:
            pkg_root = resolve_source(args.yt_dlp, wheel_sha, workdir)
            wheel_name = f"yt_dlp-{normalized(version)}-py3-none-any.whl"
        else:
            data, wheel_name = fetch_wheel(version, wheel_sha)
            pkg_root = stage_package(data, workdir)

        pairs = list(iter_patterns(pkg_root, version))

    compiled_in = []
    residual = {}
    for name, pattern in pairs:
        # A non-str _VALID_URL entry is untranspilable by definition.
        t = None if pattern is None else transpile(pattern)
        # One pattern per line: anything still carrying a raw newline (a
        # character class escaped the verbose strip) cannot be serialized,
        # and an empty result would substring-match everything.
        if t is None or "\n" in t or "\r" in t or t == "":
            residual.setdefault(name, 0)
            residual[name] += 1
        else:
            compiled_in.append((name, t))

    failed = probe_compile([t for _, t in compiled_in])
    table = []
    for i, (name, pattern) in enumerate(compiled_in):
        if i in failed:
            residual.setdefault(name, 0)
            residual[name] += 1
        else:
            table.append(pattern)

    header = (
        f"# Generated by scripts/gen-ytdlp-patterns.py from yt-dlp {version} "
        f"({wheel_name}). Do not edit.\n"
    )
    patterns = sorted(set(table))
    residual_sorted = sorted(residual)
    write_table(PATTERNS_OUT, header, patterns)
    write_table(RESIDUAL_OUT, header, residual_sorted)

    print(f"table: {len(patterns)} patterns -> {os.path.relpath(PATTERNS_OUT, REPO)}")
    print(
        f"residual: {len(residual_sorted)} extractors -> "
        f"{os.path.relpath(RESIDUAL_OUT, REPO)}"
    )
    if residual_sorted:
        print("ResidualOverrides must cover:")
        for name in residual_sorted:
            print(f"  {name}")


if __name__ == "__main__":
    main()
