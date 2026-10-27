#!/usr/bin/env sh
set -eu

repository="ActionsCommunity/multirunner"
version="${MULTIRUNNER_VERSION:-latest}"
install_dir="${MULTIRUNNER_INSTALL_DIR:-}"

usage() {
	cat <<'EOF'
Install the Multirunner CLI from a verified GitHub release.

Usage:
  install.sh [--version <tag>] [--install-dir <path>]

Options:
  --version       Release tag to install, such as v1.2.3 (default: latest)
  --install-dir   Destination directory (default: ~/.local/bin)
  -h, --help      Show this help

Environment:
  MULTIRUNNER_VERSION
  MULTIRUNNER_INSTALL_DIR
EOF
}

fail() {
	printf 'multirunner installer: %s\n' "$*" >&2
	exit 1
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			[ "$#" -ge 2 ] || fail "--version requires a value"
			version=$2
			shift 2
			;;
		--install-dir)
			[ "$#" -ge 2 ] || fail "--install-dir requires a value"
			install_dir=$2
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			fail "unknown argument: $1"
			;;
	esac
done

command -v curl >/dev/null 2>&1 || fail "curl is required"

case "$(uname -s)" in
	Linux)
		os=linux
		executable_name=multirunner
		;;
	Darwin)
		os=darwin
		executable_name=multirunner
		;;
	MINGW*|MSYS*|CYGWIN*)
		os=windows
		executable_name=multirunner.exe
		;;
	*)
		fail "unsupported operating system: $(uname -s)"
		;;
esac

case "$(uname -m)" in
	x86_64|amd64)
		arch=amd64
		;;
	arm64|aarch64)
		arch=arm64
		;;
	*)
		fail "unsupported architecture: $(uname -m)"
		;;
esac

if [ "$version" = latest ]; then
	latest_url=$(
		curl --fail --silent --show-error --location \
			--output /dev/null --write-out '%{url_effective}' \
			"https://github.com/$repository/releases/latest"
	)
	version=${latest_url##*/}
fi

case "$version" in
	v[0-9]*)
		;;
	*)
		fail "invalid release tag: $version"
		;;
esac
case "$version" in
	*[!0-9A-Za-z._-]*)
		fail "invalid release tag: $version"
		;;
esac

if [ -z "$install_dir" ]; then
	if [ "$os" = windows ] && [ -n "${LOCALAPPDATA:-}" ]; then
		if command -v cygpath >/dev/null 2>&1; then
			install_dir="$(cygpath -u "$LOCALAPPDATA")/Programs/Multirunner"
		else
			install_dir="$LOCALAPPDATA/Programs/Multirunner"
		fi
	else
		install_dir="${HOME:?HOME is required}/.local/bin"
	fi
fi

asset="multirunner_${version}_${os}_${arch}"
[ "$os" = windows ] && asset="${asset}.exe"
release_url="https://github.com/$repository/releases/download/$version"
temp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t multirunner-install)
staged=
cleanup() {
	[ -z "$staged" ] || rm -f "$staged"
	rm -rf "$temp_dir"
}
trap cleanup EXIT HUP INT TERM

printf 'Downloading Multirunner %s for %s/%s...\n' "$version" "$os" "$arch"
curl --fail --silent --show-error --location \
	"$release_url/$asset" --output "$temp_dir/$asset"
curl --fail --silent --show-error --location \
	"$release_url/SHA256SUMS.txt" --output "$temp_dir/SHA256SUMS.txt"

expected=$(
	awk -v asset="$asset" '
		length($1) == 64 && $1 !~ /[^0-9A-Fa-f]/ &&
			($2 == asset || $2 == "./" asset ||
				$2 == "*" asset || $2 == "*./" asset) {
			print tolower($1)
		}
	' "$temp_dir/SHA256SUMS.txt"
)
[ -n "$expected" ] || fail "SHA256SUMS.txt has no entry for $asset"
[ "$(printf '%s\n' "$expected" | wc -l | tr -d ' ')" = 1 ] ||
	fail "SHA256SUMS.txt has duplicate entries for $asset"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$temp_dir/$asset" | awk '{print tolower($1)}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$temp_dir/$asset" | awk '{print tolower($1)}')
else
	fail "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for $asset"

chmod 0755 "$temp_dir/$asset"
version_output=$("$temp_dir/$asset" --version 2>&1) ||
	fail "downloaded binary failed its version check"
case "$version_output" in
	*"$version"*)
		;;
	*)
		fail "downloaded binary reports an unexpected version: $version_output"
		;;
esac

mkdir -p "$install_dir"
target="$install_dir/$executable_name"
staged="$install_dir/.${executable_name}.install.$$"
backup="$target.previous"
cp "$temp_dir/$asset" "$staged"
chmod 0755 "$staged"
if [ -e "$target" ]; then
	cp -p "$target" "$backup"
fi
mv -f "$staged" "$target"

printf 'Installed %s\n' "$target"
[ -e "$backup" ] && printf 'Previous binary: %s\n' "$backup"
case ":${PATH:-}:" in
	*":$install_dir:"*)
		;;
	*)
		printf 'Add %s to PATH before running multirunner.\n' "$install_dir"
		;;
esac
printf '%s\n' "$version_output"
