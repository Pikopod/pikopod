#!/usr/bin/env bash
# Fixture provenance guard (CI). Every spec under testdata/parity/importer/specs/
# must be listed in SOURCES.md with its origin and license basis, and no fixture
# may carry a known-proprietary redistribution marker.
set -euo pipefail
cd "$(dirname "$0")/.."

SPECS_DIR=testdata/parity/importer/specs
SOURCES="$SPECS_DIR/SOURCES.md"
fail=0

if [ ! -f "$SOURCES" ]; then
  echo "FAIL: $SOURCES is missing" >&2
  exit 1
fi

# Enumerate from the filesystem, not git: an unindexed or missing repo would
# make a git-only listing pass vacuously. `find` (not a glob) so that hidden
# files and anything tucked into a subdirectory are checked too — a bare `*`
# skips dotfiles and yields directories as single unchecked entries.
specs=()
while IFS= read -r f; do
  specs+=("$f")
done < <(find "$SPECS_DIR" -type f ! -name SOURCES.md | LC_ALL=C sort)

# Vacuity guard. This counts SPECS ONLY (SOURCES.md is excluded above), so a
# directory emptied of fixtures fails instead of reporting a cheerful OK.
if [ ${#specs[@]} -eq 0 ]; then
  echo "FAIL: no spec files found under $SPECS_DIR" >&2
  exit 1
fi

for f in "${specs[@]}"; do
  base=${f#"$SPECS_DIR"/}
  # Match the backtick-delimited filename literally (grep -F). A bare
  # `grep -q "$base"` matched substrings and treated `.` as any-char, so an
  # unlisted `trimmed.json` passed on the `stripe.trimmed.json` entry.
  if ! grep -qF -- "\`$base\`" "$SOURCES"; then
    echo "FAIL: $f has no provenance entry in $SOURCES" >&2
    echo "      (expected the filename in backticks, e.g. \`$base\`)" >&2
    fail=1
  fi
done

# Known-proprietary markers. Deliberately narrow and anchored: these are
# redistribution-blocking banners, not prose. Loose substrings produce false
# positives on legitimate upstream text (Stripe describes a field as "for your
# internal use only"), which would train maintainers to ignore this gate.
markers=(
  'All [Rr]ights [Rr]eserved'
  'CONFIDENTIAL'
  'Proprietary and [Cc]onfidential'
  'documenter\.getpostman\.com'
  'NOT FOR (REDISTRIBUTION|DISTRIBUTION)'
)
for f in "${specs[@]}"; do
  for m in "${markers[@]}"; do
    if grep -qE -- "$m" "$f"; then
      echo "FAIL: $f contains a proprietary marker matching /$m/" >&2
      fail=1
    fi
  done
done

# Non-fatal: entries describing files that are no longer present. Harmless
# legally, but a stale provenance table is a misleading one.
while IFS= read -r listed; do
  [ -e "$SPECS_DIR/$listed" ] || echo "warning: $SOURCES lists \`$listed\`, which is not present" >&2
done < <(grep -oE '`[A-Za-z0-9._/-]+\.(json|yaml|yml)`' "$SOURCES" | tr -d '`' | LC_ALL=C sort -u)

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "fixture provenance guard: OK (${#specs[@]} specs)"
