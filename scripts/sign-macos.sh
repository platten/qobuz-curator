#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 1 || ! -d "$1" || "$1" != *.app ]]; then
  echo "Usage: $0 <application.app>" >&2
  exit 2
fi
: "${MACOS_CERTIFICATE_BASE64:?MACOS_CERTIFICATE_BASE64 is required}"
: "${MACOS_CERTIFICATE_PASSWORD:?MACOS_CERTIFICATE_PASSWORD is required}"
: "${MACOS_SIGN_IDENTITY:?MACOS_SIGN_IDENTITY is required}"

app_bundle="$(cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")"
temporary="$(mktemp -d)"
keychain="$temporary/signing.keychain-db"
certificate="$temporary/certificate.p12"
keychain_password="$(openssl rand -hex 24)"
original_keychains=()
while IFS= read -r original_keychain; do
  original_keychain="${original_keychain#\"}"
  original_keychain="${original_keychain%\"}"
  [[ -n "$original_keychain" ]] && original_keychains+=("$original_keychain")
done < <(security list-keychains -d user)
cleanup() {
  if [[ ${#original_keychains[@]} -gt 0 ]]; then
    security list-keychains -d user -s "${original_keychains[@]}" >/dev/null 2>&1 || true
  fi
  security delete-keychain "$keychain" >/dev/null 2>&1 || true
  [[ -z "$temporary" ]] || rm -rf -- "$temporary"
}
trap cleanup EXIT

printf '%s' "$MACOS_CERTIFICATE_BASE64" | openssl base64 -d -A -out "$certificate"
security create-keychain -p "$keychain_password" "$keychain"
security set-keychain-settings -lut 21600 "$keychain"
security unlock-keychain -p "$keychain_password" "$keychain"
security import "$certificate" -k "$keychain" -P "$MACOS_CERTIFICATE_PASSWORD" -T /usr/bin/codesign
security set-key-partition-list -S apple-tool:,apple: -s -k "$keychain_password" "$keychain"
security list-keychains -d user -s "$keychain"

codesign --force --deep --options runtime --timestamp --keychain "$keychain" --sign "$MACOS_SIGN_IDENTITY" "$app_bundle"
codesign --verify --deep --strict --verbose=2 "$app_bundle"

if [[ -n "${APPLE_ID:-}" || -n "${APPLE_TEAM_ID:-}" || -n "${APPLE_APP_PASSWORD:-}" ]]; then
  : "${APPLE_ID:?APPLE_ID is required for notarization}"
  : "${APPLE_TEAM_ID:?APPLE_TEAM_ID is required for notarization}"
  : "${APPLE_APP_PASSWORD:?APPLE_APP_PASSWORD is required for notarization}"
  archive="$temporary/qobuz-curator-desktop.zip"
  ditto -c -k --keepParent "$app_bundle" "$archive"
  xcrun notarytool submit "$archive" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" --password "$APPLE_APP_PASSWORD" --wait
  xcrun stapler staple "$app_bundle"
  xcrun stapler validate "$app_bundle"
fi
