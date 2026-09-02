#!/usr/bin/env bash
set -Eeuo pipefail

readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly project_dir="$(cd -- "$script_dir/.." && pwd)"
readonly desktop_dir="$project_dir/desktop"
readonly wails_version="v2.15.0"

platform="${1:-$(go env GOOS)/$(go env GOARCH)}"
output_dir="${2:-$project_dir/dist/desktop/${platform//\//-}}"
version="${VERSION:-}"
if [[ -z "$version" ]] && command -v git >/dev/null 2>&1; then
  version="$(git -C "$project_dir" describe --tags --always --dirty 2>/dev/null || true)"
fi
version="${version:-dev}"
if [[ ! "$version" =~ ^[A-Za-z0-9._+-]+$ ]]; then
  echo "VERSION contains unsupported characters" >&2
  exit 1
fi
if [[ "$platform" != */* ]]; then
  echo "Platform must be formatted as os/arch" >&2
  exit 1
fi
target_os="${platform%/*}"
target_arch="${platform#*/}"
case "$target_os/$target_arch" in
  windows/amd64|windows/arm64|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|darwin/universal) ;;
  *) echo "Unsupported desktop target: $platform" >&2; exit 1 ;;
esac

wails_command="${WAILS:-}"
if [[ -z "$wails_command" ]]; then
  wails_command="$(command -v wails || true)"
fi
if [[ -z "$wails_command" ]]; then
  echo "Wails $wails_version is required. Install it with:" >&2
  echo "  go install github.com/wailsapp/wails/v2/cmd/wails@$wails_version" >&2
  exit 1
fi
if ! "$wails_command" version 2>/dev/null | grep -q 'v2.15.0'; then
  echo "The desktop build requires Wails $wails_version" >&2
  exit 1
fi

tags="desktop"
build_args=(-m -s -skipbindings -clean -trimpath -platform "$platform" -tags "$tags" -ldflags "-s -w -X main.version=$version")
if [[ "$target_os" == "linux" ]]; then
  tags="desktop,webkit2_41"
  build_args=(-m -s -skipbindings -clean -trimpath -platform "$platform" -tags "$tags" -ldflags "-s -w -X main.version=$version")
fi
if [[ "$target_os" == "windows" ]]; then
  build_args+=(-nsis -webview2 embed -installscope user)
fi

echo "Building Qobuz Curator Desktop $version for $platform"
(
  cd "$desktop_dir"
  "$wails_command" build "${build_args[@]}"
)

mkdir -p "$output_dir"
case "$target_os" in
  linux)
    install -m 0755 "$desktop_dir/build/bin/qobuz-curator-desktop" "$output_dir/qobuz-curator-desktop"
    ;;
  windows)
    windows_binary="$desktop_dir/build/bin/qobuz-curator-desktop.exe"
    [[ -f "$windows_binary" ]] || windows_binary="$desktop_dir/build/bin/qobuz-curator-desktop"
    install -m 0755 "$windows_binary" "$output_dir/qobuz-curator-desktop.exe"
    installer="$(find "$desktop_dir/build/bin" -maxdepth 1 -type f -name '*installer.exe' -print -quit)"
    if [[ -z "$installer" ]]; then
      echo "Wails did not produce an NSIS installer" >&2
      exit 1
    fi
    install -m 0755 "$installer" "$output_dir/qobuz-curator-desktop-${target_arch}-installer.exe"
    ;;
  darwin)
    app_bundle="$(find "$desktop_dir/build/bin" -maxdepth 1 -type d -name '*.app' -print -quit)"
    if [[ -z "$app_bundle" ]]; then
      echo "Wails did not produce a macOS application bundle" >&2
      exit 1
    fi
    cp -R "$app_bundle" "$output_dir/"
    ;;
esac

printf '%s\n' "$version" > "$output_dir/VERSION.txt"
echo "Desktop artifacts are in $output_dir"
