#!/bin/bash
# sync-version.sh — Verify or update AppVersion in protocol.go to match git tag.
#
# Usage:
#   scripts/sync-version.sh              # check only (exit 1 on mismatch)
#   scripts/sync-version.sh --update     # auto-update AppVersion in protocol.go
#
# The git tag is expected to be in the format vMAJOR.MINOR[.PATCH][-suffix].
# Only major and minor are used for AppVersion (encoded as major<<8 | minor).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROTO_FILE="$REPO_ROOT/internal/protocol/protocol.go"

# Get version from git tag (strip leading 'v', take first two components)
RAW_TAG="$(git -C "$REPO_ROOT" describe --tags --always 2>/dev/null || echo "dev")"
BASE="$(echo "$RAW_TAG" | sed 's/^v//' | sed 's/-.*//')"

if [ "$BASE" = "dev" ]; then
    echo "sync-version: no git tag found, skipping"
    exit 0
fi

MAJOR="$(echo "$BASE" | cut -d. -f1)"
MINOR="$(echo "$BASE" | cut -d. -f2)"

if [ -z "$MAJOR" ] || [ -z "$MINOR" ]; then
    echo "sync-version: cannot parse version from tag '$RAW_TAG'"
    exit 1
fi

# Validate MAJOR and MINOR are numeric (guard against commit hashes)
if ! [[ "$MAJOR" =~ ^[0-9]+$ ]] || ! [[ "$MINOR" =~ ^[0-9]+$ ]]; then
    echo "sync-version: version components are not numeric (tag='$RAW_TAG'), skipping"
    exit 0
fi

# Compute expected hex: major << 8 | minor
EXPECTED_DEC=$(( (MAJOR << 8) | MINOR ))
EXPECTED_HEX="$(printf '0x%04X' "$EXPECTED_DEC")"
EXPECTED_LOWER="$(printf '0x%04x' "$EXPECTED_DEC")"

# Extract current AppVersion from protocol.go
CURRENT_LINE="$(grep -E '^const AppVersion uint16 = ' "$PROTO_FILE" || true)"
if [ -z "$CURRENT_LINE" ]; then
    echo "sync-version: AppVersion constant not found in $PROTO_FILE"
    exit 1
fi
CURRENT_HEX="$(echo "$CURRENT_LINE" | sed 's/.*= *//')"

# Normalize both to lowercase for comparison
CURRENT_LOWER="$(echo "$CURRENT_HEX" | tr 'A-F' 'a-f')"

if [ "$CURRENT_LOWER" = "$(echo "$EXPECTED_LOWER" | tr 'A-F' 'a-f')" ]; then
    echo "sync-version: AppVersion $CURRENT_HEX matches tag v$MAJOR.$MINOR ✓"
    exit 0
fi

# Mismatch detected
echo "sync-version: MISMATCH — AppVersion=$CURRENT_HEX, git tag=v$MAJOR.$MINOR (expected $EXPECTED_LOWER)"

if [ "${1:-}" = "--update" ]; then
    sed -i "s/^const AppVersion uint16 = .*/const AppVersion uint16 = $EXPECTED_LOWER/" "$PROTO_FILE"
    echo "sync-version: updated AppVersion to $EXPECTED_LOWER ✓"
else
    echo "sync-version: run with --update to auto-fix, or update manually"
    exit 1
fi
