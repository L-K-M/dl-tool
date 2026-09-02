#!/usr/bin/env bash
# Machine checks for the plan documents. Exit 0 = clean, 1 = at least one violation.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf 'doclint: %s\n' "$1" >&2; fail=1; }

# Emit "path:line: text" for prose only, excluding fenced code blocks so that examples
# and templates do not trip the text checks. Git's `*` in a pathspec also matches `/`.
mapfile -t DOCS < <(git ls-files -- 'docs/*.md')
prose() {   # emits "path:line:section: text"
  awk 'BEGIN { fence = sprintf("%c%c%c", 96, 96, 96) }
       FNR == 1 { inf = 0; sec = "" }
       index($0, fence) == 1 { inf = !inf; next }
       !inf && /^## / { sec = substr($0, 4) }
       !inf { printf "%s:%d:%s: %s\n", FILENAME, FNR, sec, $0 }' "${DOCS[@]}"
}

# 1. No unresolved clarifications, except in an ADR or under "## Open questions"
if prose | grep -v '^docs/decisions/' | grep -v ':Open questions: ' | grep -F 'NEEDS CLARIFICATION'; then
  note 'unresolved NEEDS CLARIFICATION outside docs/decisions/ and ## Open questions'
fi

# 2. Every task file carries the three mandatory scope-fencing sections
shopt -s nullglob
for f in docs/tasks/T*.md; do
  grep -q '^## Verification'   "$f" || note "missing Verification section: $f"
  grep -q '^## Files'          "$f" || note "missing Files section: $f"
  grep -q '^## Out of scope'   "$f" || note "missing Out of scope section: $f"
done

# 3. No hedging language in plan documents
if prose | grep -v '^docs/decisions/' \
   | grep -iE '\b(TBD|we could|you could also|might want|maybe|probably)\b'; then
  note 'hedging language in a plan document'
fi

# 4. No absolute links back into this repository; relative links only
if prose | grep -iE 'https://github\.com/L-K-M/dl-tool/(blob|tree)/'; then
  note 'absolute self-link; use a relative path'
fi

# 5. Relative links and anchors resolve
if command -v lychee >/dev/null 2>&1; then
  lychee --offline --include-fragments docs README.md AGENTS.md || note 'broken relative link or anchor'
else
  note 'lychee is not installed; run make setup'
fi

exit "$fail"
