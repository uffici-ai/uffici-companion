#!/bin/sh
# Uffici companion installer.
#
# One line on the machine that runs your harness:
#
#   curl -fsSL https://raw.githubusercontent.com/uffici-ai/uffici-companion/main/install.sh \
#     | sh -s -- --harness hermes --gateway-url https://agents.uffici.ai
#
# This script does exactly one thing itself: it fetches the companion binary for
# this machine from the latest release, verifies it against the release's
# checksum list, and puts it on your PATH. Everything else — the service, the
# pairing, your harness's plugin, the doctor — is `uffici setup`, which this
# script hands every argument to. `uffici setup` says what it is about to do
# before each step; `--dry-run` makes it only say, and then this script installs
# nothing either.
#
# Environment:
#   UFFICI_INSTALL_PREFIX   where the binary goes (default ~/.local; the binary is <prefix>/bin/uffici)
#   UFFICI_VERSION          pin a release (default: the latest)
#   UFFICI_GITHUB_TOKEN     a GitHub token, only while the repository is private; never printed,
#                           never placed on a command line
#   UFFICI_GATEWAY_URL      the agents host, if not given as --gateway-url
#
# POSIX sh, so it runs under dash, bash, zsh and busybox alike. No bashisms. The
# whole body is one routine invoked on the last line, so a download cut short
# defines nothing that runs.
set -eu

say() { printf '%s\n' "uffici-install: $*"; }
die() { printf '%s\n' "uffici-install: $*" >&2; exit "${2:-1}"; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is needed and not on the PATH" 2; }

main() {
  repo="${UFFICI_COMPANION_REPO:-uffici-ai/uffici-companion}"
  base="${UFFICI_RELEASES_BASE_URL:-https://github.com}"
  api="${UFFICI_RELEASES_API_URL:-https://api.github.com}"
  version="${UFFICI_VERSION:-}"
  prefix="${UFFICI_INSTALL_PREFIX:-$HOME/.local}"
  token="${UFFICI_GITHUB_TOKEN:-}"

  need curl
  need tar
  need uname
  need mktemp

  # ── What was asked ──────────────────────────────────────────────────────
  dry_run=no; needs_answer=yes
  for arg in "$@"; do
    case "$arg" in
      --dry-run) dry_run=yes ;;
      --yes|--no-pair) needs_answer=no ;;
    esac
  done
  [ "$dry_run" = yes ] && needs_answer=no

  # ── This machine ────────────────────────────────────────────────────────
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) die "$(uname -s) is not supported; the companion runs on Linux and macOS" 2 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "$(uname -m) is not a supported architecture" 2 ;;
  esac
  case "$os/$arch" in
    linux/amd64|linux/arm64|darwin/arm64) ;;
    *) die "$os/$arch has no release; supported: linux/amd64, linux/arm64, darwin/arm64" 2 ;;
  esac
  asset="uffici_${os}_${arch}.tar.gz"

  work=$(mktemp -d "${TMPDIR:-/tmp}/uffici-install.XXXXXX")
  trap 'rm -rf "$work"' EXIT
  trap 'rm -rf "$work"; exit 130' INT TERM

  # Pairing asks "install this key?" on stdin. Under `curl | sh` stdin is the
  # pipe the script came down, already at its end, and an empty answer is a no
  # — after an administrator has already approved the code. So the answer is
  # read from the terminal, and with no terminal at all the run is refused
  # before anything is downloaded, naming the two ways out.
  answer_from=""
  if [ "$needs_answer" = yes ] && [ ! -t 0 ]; then
    if (: </dev/tty) 2>/dev/null; then
      answer_from=/dev/tty
    else
      die "pairing needs a terminal to confirm which agent the key is for, and this run has none. Re-run with --yes to skip the question, with --no-pair if the key is already in place, or run \`uffici setup\` yourself from a terminal." 2
    fi
  fi

  if [ -n "$version" ]; then say "fetching uffici $version for $os/$arch from $repo"; else say "fetching the latest uffici for $os/$arch from $repo"; fi
  fetch checksums.txt "$work/checksums.txt"
  fetch "$asset" "$work/$asset"

  # ── Verifying ───────────────────────────────────────────────────────────
  expected=$(grep -E "[[:space:]]\*?$asset\$" "$work/checksums.txt" | head -n 1 | cut -d' ' -f1)
  [ -n "$expected" ] || die "checksums.txt of the release does not list $asset — refusing an asset the release did not vouch for"
  actual=$(sha256_of "$work/$asset")
  [ "$actual" = "$expected" ] || die "$asset does not match its published checksum — refusing to install it"
  say "checksum verified"

  mkdir -p "$work/unpacked"
  tar -xzf "$work/$asset" -C "$work/unpacked"
  [ -f "$work/unpacked/uffici" ] || die "the archive holds no uffici binary"
  chmod 0755 "$work/unpacked/uffici"

  # ── A dry run installs nothing: the unpacked binary only tells ──────────
  if [ "$dry_run" = yes ]; then
    say "dry run: would install $prefix/bin/uffici ($("$work/unpacked/uffici" -version 2>/dev/null || echo 'version unknown')), then run: uffici setup $*"
    "$work/unpacked/uffici" setup "$@"
    return $?
  fi

  # ── Installing ──────────────────────────────────────────────────────────
  mkdir -p "$prefix/bin"
  cp "$work/unpacked/uffici" "$prefix/bin/uffici.new"
  mv -f "$prefix/bin/uffici.new" "$prefix/bin/uffici"
  say "installed $prefix/bin/uffici ($("$prefix/bin/uffici" -version 2>/dev/null || echo 'version unknown'))"
  case ":$PATH:" in
    *":$prefix/bin:"*) ;;
    *) say "note: $prefix/bin is not on your PATH; add it to run uffici by name" ;;
  esac

  # ── Handing over ────────────────────────────────────────────────────────
  # `exec` replaces this shell, so the EXIT trap would never run: clean up
  # first, then drop the traps.
  say "handing over to: uffici setup $*"
  rm -rf "$work"
  trap - EXIT INT TERM
  if [ -n "$answer_from" ]; then
    exec "$prefix/bin/uffici" setup "$@" <"$answer_from"
  fi
  exec "$prefix/bin/uffici" setup "$@"
}

# fetch <asset name> <destination file>
#
# Without a token: the public download URL. With one (a private repository):
# the release listing through the API, then the asset by its API URL. 🔴 The
# token reaches curl through its config on stdin (`-K -`), never as an
# argument: an argument is readable by every local user in the process list.
fetch() {
  if [ -n "$token" ]; then
    if [ -n "$version" ]; then rel="tags/v$version"; else rel="latest"; fi
    listing=$(printf 'header = "Authorization: Bearer %s"\nheader = "Accept: application/vnd.github+json"\n' "$token" \
      | curl -fsSL -K - "$api/repos/$repo/releases/$rel") \
      || die "the release listing of $repo could not be read (is the token right, is there a release?)"
    # Flatten the JSON, drop the nested objects an asset carries (its
    # uploader), then take the `url` of the object whose `name` is the asset.
    url=$(printf '%s' "$listing" | tr -d '\n ' | sed -E 's/"[A-Za-z_]+":\{[^{}]*\}//g' \
      | sed -n 's/.*"url":"\([^"]*\)"[^{}]*"name":"'"$1"'".*/\1/p')
    [ -n "$url" ] || die "the release of $repo has no asset named $1"
    case "$url" in
      "$api"/repos/*) ;;
      *) die "the release listing names an asset outside $api — refusing it" ;;
    esac
    printf 'header = "Authorization: Bearer %s"\nheader = "Accept: application/octet-stream"\n' "$token" \
      | curl -fsSL -K - -o "$2" "$url" \
      || die "downloading $1 failed"
  else
    if [ -n "$version" ]; then dl_path="releases/download/v$version/$1"; else dl_path="releases/latest/download/$1"; fi
    curl -fsSL -o "$2" "$base/$repo/$dl_path" \
      || die "downloading $1 from $repo failed (is there a release, and is the repository public? set UFFICI_GITHUB_TOKEN for a private one)"
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  else die "neither sha256sum nor shasum is available to verify the download" 2
  fi
}

main "$@"
