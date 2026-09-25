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
    meta = json.load(
        urllib.request.urlopen(
            f"https://pypi.org/pypi/yt-dlp/{normalized(version)}/json", timeout=60
        )
    )
    for entry in meta["urls"]:
        if entry["filename"].endswith("-py3-none-any.whl"):
            return entry["url"], entry["filename"]
    die(f"PyPI release yt-dlp {normalized(version)} carries no py3-none-any wheel")


def fetch_wheel(version, want_sha256):
    url, filename = wheel_url(version)
    data = urllib.request.urlopen(url, timeout=300).read()
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
            if isinstance(pattern, str):
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


def transpile(pattern):
    """Python re -> RE2. Returns the pattern, or None to mark it residual.

    The verbose flag is pattern-initial in every corpus case: strip x from a
    leading flag group while preserving co-flags, then drop whitespace and
    comments inside the verbose region only — the whole pattern for a bare
    (?x), the interior of a leading (?x:…) group that reaches end-of-pattern.
    Anything else lands residual rather than guessing.
    """
    if "(?x" not in pattern:
        return rewrite_captures(pattern)

    m = re.match(r"\(\?([a-zA-Z]+)(:|\))", pattern)
    if not m or "x" not in m.group(1):
        return None
    coflags = m.group(1).replace("x", "")

    if m.group(2) == ")":
        head = f"(?{coflags})" if coflags else ""
        return rewrite_captures(strip_verbose(head + pattern[m.end() :]))

    close = close_paren(pattern, 0)
    if close != len(pattern) - 1:
        return None
    head = f"(?{coflags}:" if coflags else "(?:"
    return rewrite_captures(head + strip_verbose(pattern[m.end() : close]) + ")")


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
        return {int(line) for line in out.stdout.split()}
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
        t = transpile(pattern)
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
    write_table(PATTERNS_OUT, header, sorted(set(table)))
    write_table(RESIDUAL_OUT, header, sorted(residual))

    print(f"table: {len(set(table))} patterns -> {os.path.relpath(PATTERNS_OUT, REPO)}")
    print(
        f"residual: {len(residual)} extractors -> {os.path.relpath(RESIDUAL_OUT, REPO)}"
    )
    print("ResidualOverrides must cover:")
    for name in sorted(residual):
        print(f"  {name}")


if __name__ == "__main__":
    main()
