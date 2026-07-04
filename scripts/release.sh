#!/usr/bin/env bash
# release.sh — build a versioned D BOX release + regenerate the update manifest.
#
#   scripts/release.sh 1.1.0
#
# Produces in dist/:
#   DBox.exe                 raw executable  (what the in-app updater downloads)
#   DBox-Setup-<ver>.exe     full installer  (what new users download from the site)
#   latest.json              update manifest (upload next to your site / release)
#
# The version is stamped into the binary (version.Version) AND the manifest, so
# the running app can compare itself to the latest. It also bakes in the
# manifest URL (version.ManifestURL) so update checks know where to look.
#
# After it runs, do two uploads (see DEPLOY.md):
#   1. Attach DBox.exe + DBox-Setup-<ver>.exe to a GitHub Release tagged v<ver>.
#   2. Publish website/ (with latest.json) to GitHub Pages / Netlify.
set -euo pipefail
cd "$(dirname "$0")/.."

VER="${1:-}"
if [[ -z "$VER" ]]; then echo "usage: scripts/release.sh <version>   e.g. 1.1.0" >&2; exit 1; fi
if ! [[ "$VER" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then echo "version must be X.Y.Z" >&2; exit 1; fi

# ---- EDIT THESE for your accounts (once) ----------------------------------
GITHUB_USER="66lexel999"
GITHUB_REPO="dbox"
# Where latest.json will be reachable (GitHub Pages default shown). If you host
# the site on Netlify/your domain, point this at that URL instead.
MANIFEST_URL="https://${GITHUB_USER}.github.io/${GITHUB_REPO}/latest.json"
# Direct download URLs for the two assets on the GitHub Release for this version.
EXE_URL="https://github.com/${GITHUB_USER}/${GITHUB_REPO}/releases/download/v${VER}/DBox.exe"
INSTALLER_URL="https://github.com/${GITHUB_USER}/${GITHUB_REPO}/releases/download/v${VER}/DBox-Setup-${VER}.exe"
# ---------------------------------------------------------------------------

export PATH="/c/Program Files/Go/bin:$PATH"
ISCC="${ISCC:-$HOME/AppData/Local/Programs/Inno Setup 6/ISCC.exe}"
# Where your yt-dlp.exe / aria2c.exe live (override with DBOX_TOOLS=...). They're
# staged into ./tools/ so the installer bundles them without a hardcoded path.
TOOLS_SRC="${DBOX_TOOLS:-$HOME/Downloads/flowerX/Programs}"

echo "$VER" > VERSION
mkdir -p dist build/tools
for t in yt-dlp.exe aria2c.exe; do
  [[ -f "$TOOLS_SRC/$t" ]] && cp -f "$TOOLS_SRC/$t" "build/tools/$t" || echo "note: $t not found in $TOOLS_SRC (installer will omit it)"
done

echo ">> building DBox.exe  (v$VER)"
LDFLAGS="-H windowsgui -X myidm/internal/version.Version=${VER} -X myidm/internal/version.ManifestURL=${MANIFEST_URL}"
go build -tags "desktop production" -ldflags "$LDFLAGS" -o bin/DBox.exe ./cmd/myidm
cp -f bin/DBox.exe DBox.exe
cp -f bin/DBox.exe dist/DBox.exe

# DPI manifest sanity check (must be embedded exactly once).
if [[ "$(grep -c permonitorv2 bin/DBox.exe)" != "1" ]]; then
  echo "!! DPI manifest missing from the build" >&2; exit 1
fi

echo ">> building installer"
# MSYS2_ARG_CONV_EXCL stops git-bash from mangling the /D... option into a path.
MSYS2_ARG_CONV_EXCL='*' "$ISCC" "/DMyAppVersion=${VER}" build/installer.iss >/dev/null

echo ">> hashing DBox.exe (sha256 -> manifest)"
SHA="$(sha256sum dist/DBox.exe | cut -d' ' -f1)"
DATE="$(date +%Y-%m-%d)"

# Read release notes from CHANGELOG-<ver>.md if present, else a placeholder.
NOTES_FILE="dist/notes-${VER}.txt"
if [[ -f "$NOTES_FILE" ]]; then NOTES="$(cat "$NOTES_FILE")"; else NOTES="See the website for what's new in v${VER}."; fi

# Emit latest.json (JSON-escape the notes via python for safety).
python - "$VER" "$EXE_URL" "$INSTALLER_URL" "$SHA" "$DATE" "$NOTES" > dist/latest.json <<'PY'
import json, sys
ver, url, inst, sha, date, notes = sys.argv[1:7]
print(json.dumps({
    "version": ver, "url": url, "installerUrl": inst,
    "sha256": sha, "date": date, "notes": notes, "mandatory": False
}, indent=2))
PY
cp -f dist/latest.json website/latest.json 2>/dev/null || true

echo
echo "== release v$VER ready in dist/ =="
ls -la dist/DBox.exe "dist/DBox-Setup-${VER}.exe" dist/latest.json
echo
echo "sha256(DBox.exe) = $SHA"
echo
echo "Next (see DEPLOY.md):"
echo "  1) gh release create v$VER dist/DBox.exe \"dist/DBox-Setup-${VER}.exe\" -t \"D BOX v$VER\" -n \"\$(cat $NOTES_FILE 2>/dev/null)\""
echo "  2) publish website/ (with latest.json) to GitHub Pages / Netlify"
