#!/usr/bin/env bash
set -Eeuo pipefail

readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly project_dir="$(cd -- "$script_dir/.." && pwd)"
readonly output_dir="$project_dir/dist"
readonly cyclonedx_version="v1.12.0"

version="${VERSION:-}"
if [[ -z "$version" ]] && command -v git >/dev/null 2>&1; then
  version="$(git -C "$project_dir" describe --tags --always --dirty 2>/dev/null || true)"
fi
version="${version:-dev}"
if [[ ! "$version" =~ ^[A-Za-z0-9._+-]+$ ]]; then
  echo "VERSION contains unsupported characters" >&2
  exit 1
fi

staging_dir="$(mktemp -d)"
tool_dir="$(mktemp -d)"
cleanup() {
	[[ -z "$staging_dir" ]] || rm -rf -- "$staging_dir"
	[[ -z "$tool_dir" ]] || rm -rf -- "$tool_dir"
}
trap cleanup EXIT

targets=(
  linux/amd64
  linux/arm64
  windows/amd64
  windows/arm64
  darwin/amd64
  darwin/arm64
)

echo "Building Qobuz Curator $version"
for target in "${targets[@]}"; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  suffix=""
  [[ "$target_os" == "windows" ]] && suffix=".exe"
  artifact="qobuz-curator-${target_os}-${target_arch}${suffix}"
  echo "  -> $artifact"
  (
    cd "$project_dir"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
      go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=$version" \
      -o "$staging_dir/$artifact" .
  )
done

printf '%s\n' "$version" > "$staging_dir/VERSION.txt"
cp "$project_dir/LICENSE" "$staging_dir/LICENSE"
cp "$project_dir/README.md" "$staging_dir/README.md"

echo "Generating CycloneDX SBOM"
GOBIN="$tool_dir" go install "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$cyclonedx_version"
"$tool_dir/cyclonedx-gomod" bin -std -json -version "$version" \
  -output "$staging_dir/qobuz-curator.cdx.json" \
  "$staging_dir/qobuz-curator-linux-amd64"

(
  cd "$staging_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    hash_command=(sha256sum)
  elif command -v shasum >/dev/null 2>&1; then
    hash_command=(shasum -a 256)
  else
    echo "Neither sha256sum nor shasum is available" >&2
    exit 1
  fi
  : > SHA256SUMS
  LC_ALL=C
  for file in *; do
    [[ -f "$file" && "$file" != "SHA256SUMS" ]] || continue
    digest="$("${hash_command[@]}" "$file" | awk '{print $1}')"
    printf '%s  %s\n' "$digest" "$file" >> SHA256SUMS
  done
)

# The destination is fixed relative to the repository. Validate it before the
# only recursive removal performed by this script.
if [[ "$output_dir" != "$project_dir/dist" || "$output_dir" == "/" ]]; then
  echo "Refusing unsafe output directory: $output_dir" >&2
  exit 1
fi
rm -rf -- "$output_dir"
mv -- "$staging_dir" "$output_dir"
staging_dir=""

echo "Release artifacts are in $output_dir"
