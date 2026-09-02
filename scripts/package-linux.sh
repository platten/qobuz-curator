#!/usr/bin/env bash
set -Eeuo pipefail

readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly project_dir="$(cd -- "$script_dir/.." && pwd)"

if [[ $# -ne 4 ]]; then
  echo "Usage: $0 <binary> <amd64|arm64> <version> <output-directory>" >&2
  exit 2
fi
binary="$(realpath -- "$1")"
arch="$2"
version="$3"
output_dir="$(realpath -m -- "$4")"
if [[ ! -f "$binary" || ! -x "$binary" ]]; then
  echo "Desktop binary is missing or not executable: $binary" >&2
  exit 1
fi
if [[ "$arch" != "amd64" && "$arch" != "arm64" ]]; then
  echo "Linux architecture must be amd64 or arm64" >&2
  exit 1
fi
if [[ ! "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([._+-][A-Za-z0-9.-]+)?$ ]]; then
  echo "Linux packages require a semantic version" >&2
  exit 1
fi
package_version="${version#v}"
mkdir -p "$output_dir"

staging="$(mktemp -d)"
cleanup() {
  [[ -z "$staging" ]] || rm -rf -- "$staging"
}
trap cleanup EXIT

deb_root="$staging/deb"
install -d -m 0755 "$deb_root/DEBIAN" "$deb_root/opt/qobuz-curator" "$deb_root/usr/share/applications" "$deb_root/usr/share/icons/hicolor/512x512/apps"
install -m 0755 "$binary" "$deb_root/opt/qobuz-curator/qobuz-curator-desktop"
install -m 0644 "$project_dir/packaging/linux/qobuz-curator.desktop" "$deb_root/usr/share/applications/qobuz-curator.desktop"
install -m 0644 "$project_dir/desktop/build/appicon.png" "$deb_root/usr/share/icons/hicolor/512x512/apps/qobuz-curator.png"
sed -e "s/@VERSION@/$package_version/g" -e "s/@ARCH@/$arch/g" "$project_dir/packaging/linux/deb-control.in" > "$deb_root/DEBIAN/control"
dpkg-deb --root-owner-group --build "$deb_root" "$output_dir/qobuz-curator-desktop_${package_version}_${arch}.deb"

appdir="$staging/Qobuz_Curator.AppDir"
install -d -m 0755 "$appdir/usr/bin"
install -m 0755 "$binary" "$appdir/usr/bin/qobuz-curator-desktop"
install -m 0755 "$project_dir/packaging/linux/AppRun" "$appdir/AppRun"
install -m 0644 "$project_dir/packaging/linux/qobuz-curator.desktop" "$appdir/qobuz-curator.desktop"
install -m 0644 "$project_dir/desktop/build/appicon.png" "$appdir/qobuz-curator.png"

appimagetool="${APPIMAGETOOL:-$(command -v appimagetool || true)}"
if [[ -z "$appimagetool" || ! -x "$appimagetool" ]]; then
  echo "APPIMAGETOOL must name an executable appimagetool 1.9.1 binary" >&2
  exit 1
fi
appimage_runtime="${APPIMAGE_RUNTIME:-}"
if [[ -z "$appimage_runtime" || ! -f "$appimage_runtime" ]]; then
  echo "APPIMAGE_RUNTIME must name the pinned AppImage type-2 runtime" >&2
  exit 1
fi
appimage_arch="x86_64"
[[ "$arch" == "arm64" ]] && appimage_arch="aarch64"
appimagetool_args=()
[[ "$appimagetool" == *.AppImage ]] && appimagetool_args+=(--appimage-extract-and-run)
ARCH="$appimage_arch" VERSION="$package_version" "$appimagetool" "${appimagetool_args[@]}" --no-appstream --runtime-file "$appimage_runtime" "$appdir" "$output_dir/qobuz-curator-desktop-${version}-linux-${arch}.AppImage"
chmod 0755 "$output_dir/qobuz-curator-desktop-${version}-linux-${arch}.AppImage"

echo "Linux packages are in $output_dir"
