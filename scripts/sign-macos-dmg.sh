#!/bin/bash
# Sign a GitHub-built MKRouter DMG using a local Developer ID identity.
set -euo pipefail

if [[ $# -ne 2 || -z "${APPLE_SIGNING_IDENTITY:-}" ]]; then
  echo 'Usage: APPLE_SIGNING_IDENTITY="Developer ID Application: ..." bash scripts/sign-macos-dmg.sh INPUT.dmg OUTPUT.dmg' >&2
  exit 1
fi

input="$1"
output="$2"
[[ -f "$input" ]] || { echo "Missing input: $input" >&2; exit 1; }
[[ ! -e "$output" && ! -e "$output.sha256" ]] || { echo "Output already exists: $output" >&2; exit 1; }
mkdir -p "$(dirname "$output")"
output="$(cd "$(dirname "$output")" && pwd)/$(basename "$output")"
work=$(mktemp -d "${TMPDIR:-/tmp}/mkrouter-sign.XXXXXX")
mounted=false
cleanup() {
  if [[ "$mounted" == true ]]; then
    hdiutil detach "$work/mount" >/dev/null || true
  fi
  # Keep intermediate files for inspection or a notarization follow-up.
  echo "Signing work directory: $work"
}
trap cleanup EXIT

hdiutil attach "$input" -readonly -nobrowse -mountpoint "$work/mount"
mounted=true
app="$work/staging/MKRouter.app"
[[ -d "$work/mount/MKRouter.app" ]] || { echo 'MKRouter.app is missing' >&2; exit 1; }
ditto "$work/mount" "$work/staging"
hdiutil detach "$work/mount"
mounted=false

identifier=$(/usr/libexec/PlistBuddy -c 'Print CFBundleIdentifier' "$app/Contents/Info.plist")
[[ "$identifier" == cc.minki.router ]] || { echo "Unexpected bundle: $identifier" >&2; exit 1; }
for binary in mkrouter-backend mkrouter-desktop; do
  file "$app/Contents/MacOS/$binary" | /usr/bin/grep -q 'Mach-O' || { echo "Missing Mach-O: $binary" >&2; exit 1; }
done

# Sign nested code first, then seal the application. Do not use --deep to sign.
codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$app/Contents/MacOS/mkrouter-backend"
codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$app"
codesign --verify --deep --strict --verbose=2 "$app"
codesign --display --verbose=4 "$app"

hdiutil create -volname MKRouter -srcfolder "$work/staging" -format UDZO "$output"
codesign --force --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$output"
codesign --verify --strict --verbose=2 "$output"
codesign --display --verbose=4 "$output"
(
  cd "$(dirname "$output")"
  shasum -a 256 "$(basename "$output")" > "$(basename "$output").sha256"
)
echo "Signed (not notarized): $output"
