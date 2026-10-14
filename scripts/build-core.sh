#!/usr/bin/env bash
set -euo pipefail

# Builds in an isolated, newly created output directory; never installs a core.
core_version=v1.14.3
core_commit=7054cac5124465ffdb145870d7c01885566711b1
go_bin=${GO_BIN:-go}
output_dir=${1:-${BUILD_OUT:-}}

if [[ -z "$output_dir" || $# -gt 1 ]]; then
    printf 'Usage: %s <new-output-directory>\n' "$0" >&2
    exit 2
fi
if ! go_bin=$(command -v "$go_bin"); then
    printf 'Go is required; set GO_BIN to an existing Go executable.\n' >&2
    exit 1
fi
[[ "$go_bin" = /* ]] || go_bin="$PWD/$go_bin"
if ! command -v git >/dev/null 2>&1; then
    printf 'git is required.\n' >&2
    exit 1
fi
if [[ -e "$output_dir" || -L "$output_dir" ]]; then
    printf 'Refusing to use an existing output path: %s\n' "$output_dir" >&2
    exit 1
fi

# Avoid automatic toolchain installation; the caller supplies a compatible Go.
export GOTOOLCHAIN=local
mkdir -p -- "$output_dir"
output_dir=$(cd -- "$output_dir" && pwd -P)
build_dir=$(mktemp -d "$output_dir/.core-build.XXXXXX")
trap 'rm -rf -- "$build_dir"' EXIT

git clone --depth 1 --branch "$core_version" --single-branch \
    https://github.com/SagerNet/sing-box.git "$build_dir/source"
source_dir=$build_dir/source
source_commit=$(git -C "$source_dir" rev-parse HEAD)
if [[ $source_commit != "$core_commit" ]]; then
    printf 'Core source commit mismatch: expected %s, got %s.\n' "$core_commit" "$source_commit" >&2
    exit 1
fi
tags=$(tr -d '[:space:]' < "$source_dir/release/DEFAULT_BUILD_TAGS")
case ",$tags," in
    *,with_v2ray_api,*) ;;
    *) tags="$tags,with_v2ray_api" ;;
esac
# cronet-go's CGO-free implementation is selected explicitly by this tag.
if [[ $("$go_bin" env CGO_ENABLED) == 0 ]]; then
    case ",$tags," in
        *,with_purego,*) ;;
        *) tags="$tags,with_purego" ;;
    esac
fi
ldflags=$(cat "$source_dir/release/LDFLAGS")
(
    cd -- "$source_dir"
    "$go_bin" build -trimpath -tags "$tags" \
        -ldflags "-X github.com/sagernet/sing-box/constant.Version=${core_version#v} $ldflags -s -w -buildid=" \
        -o "$output_dir/sing-box" ./cmd/sing-box
)
cp -- "$source_dir/LICENSE" "$output_dir/LICENSE"
{
    printf 'version=%s\n' "$core_version"
    printf 'source=https://github.com/SagerNet/sing-box/tree/%s\n' "$core_version"
    printf 'commit=%s\n' "$source_commit"
    printf 'tags=%s\n' "$tags"
    printf 'toolchain=%s\n' "$("$go_bin" version)"
} > "$output_dir/BUILD-INFO.txt"
printf 'Built core in %s; no installed binary was changed.\n' "$output_dir"
