#!/bin/bash
# Generate Windows version info resource (.syso) for PE file properties.
# Usage: gen-versioninfo.sh <target-dir> <binary-name> <description> <version> [arch: 64|32]
set -euo pipefail

DIR="$1"
BIN="$2"
DESC="$3"
VER="$4"
ARCH="${5:-64}"

command -v goversioninfo >/dev/null 2>&1 || {
  echo "Error: goversioninfo not found. Install with: go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest"
  exit 1
}

# Strip leading 'v' and git suffix (e.g. "v1.9.0-13-g39be43c-dirty" -> "1.9.0")
BASE=$(echo "$VER" | sed 's/^v//' | sed 's/-.*//')
MAJOR=$(echo "$BASE" | cut -d. -f1)
MINOR=$(echo "$BASE" | cut -d. -f2)
PATCH=$(echo "$BASE" | cut -d. -f3 | cut -d+ -f1)
[ -z "$PATCH" ] && PATCH=0

# Extract semantic version (e.g., "1.9.0" from "v1.9.0-13-g39be43c-dirty")
SEMVER="$MAJOR.$MINOR.$PATCH"

JSON=$(cat <<EOF
{
  "FixedFileInfo": {
    "FileVersion": {
      "Major": $MAJOR,
      "Minor": $MINOR,
      "Patch": $PATCH,
      "Build": 0
    },
    "ProductVersion": {
      "Major": $MAJOR,
      "Minor": $MINOR,
      "Patch": $PATCH,
      "Build": 0
    },
    "FileFlagsMask": "3f",
    "FileFlags ": "00",
    "FileOS": "040004",
    "FileType": "01",
    "FileSubType": "00"
  },
  "StringFileInfo": {
    "Comments": "GameTunnel",
    "CompanyName": "",
    "FileDescription": "$DESC",
    "FileVersion": "$SEMVER",
    "InternalName": "$BIN",
    "LegalCopyright": "",
    "LegalTrademarks": "",
    "OriginalFilename": "$BIN",
    "ProductName": "GameTunnel",
    "ProductVersion": "$SEMVER"
  },
  "VarFileInfo": {
    "Translation": {
      "LangID": "0804",
      "CharsetID": "04B0"
    }
  }
}
EOF
)

echo "$JSON" > "$DIR/versioninfo.json"

FLAG="-64"
[ "$ARCH" = "32" ] && FLAG="-64=false"

goversioninfo $FLAG -o "$DIR/versioninfo.syso" "$DIR/versioninfo.json"
rm -f "$DIR/versioninfo.json"
