#!/bin/sh
# Uffici companion installer.
#
# One line on the machine that runs your harness — the portal gives it to you,
# with a pairing code in it:
#
#   curl -fsSL https://raw.githubusercontent.com/uffici-ai/uffici-companion/main/install.sh \
#     | sh -s -- --harness hermes --gateway-url https://agents.uffici.ai --pairing-code XXXX-XXXX
#
# This script does exactly one thing itself: it fetches the companion binary for
# this machine — the release your agents gateway names, else the latest — verifies
# it against the release's checksum list, and puts it on your PATH. Everything else — the service, the
# pairing, your harness's plugin, the doctor — is `uffici setup`, which this
# script hands every argument to. `uffici setup` says what it is about to do
# before each step; `--dry-run` makes it only say, and then this script installs
# nothing either.
#
# Environment:
#   UFFICI_INSTALL_PREFIX   where the binary goes (default ~/.local; the binary is <prefix>/bin/uffici)
#   UFFICI_VERSION          pin a release (default: the one the agents gateway names, else the latest)
#   UFFICI_GITHUB_TOKEN     a GitHub token, only while the repository is private; never printed,
#                           never placed on a command line
#   UFFICI_GATEWAY_URL      the agents gateway, if not given as --gateway-url (default: the one a
#                           previous setup recorded, else https://agents.uffici.ai)
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
  version_from="UFFICI_VERSION"
  default_gateway="https://agents.uffici.ai"
  prefix="${UFFICI_INSTALL_PREFIX:-$HOME/.local}"
  token="${UFFICI_GITHUB_TOKEN:-}"

  need curl
  need tar
  need uname
  need mktemp

  # ── What was asked ──────────────────────────────────────────────────────
  # A pairing code from the portal asks nothing — the administrator who issued
  # it chose the agent — so, like --yes, it needs no terminal. The code is a
  # secret: `shown` is the argument list with it masked, and it is the only
  # form of the arguments this script ever prints.
  dry_run=no; needs_answer=yes; shown=""; mask_next=no
  gateway=""; version_arg=""; home_arg=""; take=""
  for arg in "$@"; do
    case "$take" in
      gateway) gateway="$arg" ;;
      version) version_arg="$arg" ;;
      home) home_arg="$arg" ;;
    esac
    take=""
    case "$arg" in
      --dry-run) dry_run=yes ;;
      --yes|--no-pair|--pairing-code|--pairing-code=*) needs_answer=no ;;
      --gateway-url) take=gateway ;;
      --gateway-url=*) gateway="${arg#--gateway-url=}" ;;
      --home) take=home ;;
      --home=*) home_arg="${arg#--home=}" ;;
      --version) take=version ;;
      --version=*) version_arg="${arg#--version=}" ;;
    esac
    if [ "$mask_next" = yes ]; then
      arg="****-****"; mask_next=no
    else
      case "$arg" in
        --pairing-code) mask_next=yes ;;
        --pairing-code=*) arg="--pairing-code=****-****" ;;
      esac
    fi
    shown="${shown:+$shown }$arg"
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

  # ── Which gateway, and which release it names ───────────────────────────
  # --gateway-url, UFFICI_GATEWAY_URL, the one a previous setup recorded, the
  # production default — named, with where it came from, before anything is
  # downloaded. The gateway names the release its machines run; `uffici setup`
  # is handed the same version, so both agree.
  if [ -n "$gateway" ]; then gateway_from="from --gateway-url"
  elif [ -n "${UFFICI_GATEWAY_URL:-}" ]; then gateway="$UFFICI_GATEWAY_URL"; gateway_from="from UFFICI_GATEWAY_URL"
  else
    # The record setup will read: its --home, else UFFICI_HOME, else the default.
    record="${home_arg:-${UFFICI_HOME:-$HOME/.uffici}}/setup.json"
    if [ -f "$record" ]; then
      gateway=$(sed -n 's/.*"gatewayUrl"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$record" | head -n 1)
    fi
    if [ -n "$gateway" ]; then gateway_from="recorded in setup.json"; else gateway="$default_gateway"; gateway_from="the default"; fi
  fi
  gateway="${gateway%/}"
  say "agents gateway: $gateway ($gateway_from)"

  if [ -n "$version_arg" ]; then version="$version_arg"; version_from="--version"; fi
  if [ -n "$version" ]; then
    say "version: $version, pinned with $version_from"
  else
    ask_gateway
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

  # The version travels to setup, so the plugin follows the same release — to a
  # binary that knows the flag: a gateway rolled back to an older release names
  # one whose setup would refuse it.
  if [ -n "$version" ] && [ -z "$version_arg" ]; then
    if "$work/unpacked/uffici" setup -h 2>&1 | grep -q '^  -version'; then
      set -- "$@" --version "$version"
      shown="$shown --version $version"
    else
      say "uffici $version does not take setup --version; its setup chooses the plugin's version itself"
    fi
  fi

  # ── A dry run installs nothing: the unpacked binary only tells ──────────
  if [ "$dry_run" = yes ]; then
    say "dry run: would install $prefix/bin/uffici ($("$work/unpacked/uffici" -version 2>/dev/null || echo 'version unknown')), then run: uffici setup $shown"
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
  say "handing over to: uffici setup $shown"
  rm -rf "$work"
  trap - EXIT INT TERM
  if [ -n "$answer_from" ]; then
    exec "$prefix/bin/uffici" setup "$@" <"$answer_from"
  fi
  exec "$prefix/bin/uffici" setup "$@"
}

# ask_gateway asks the agents gateway which release its machines run
# (`GET <gateway>/v1/release`) and sets `version` to it; on no answer, or no
# version named, `version` stays empty — the latest release — and ONE line says
# which path chose it and why. 🔴 The GitHub token is never sent to it.
ask_gateway() {
  case "$gateway" in
    https://*|http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*) ;;
    *) say "version: the latest release — the agents gateway $gateway is not an https origin"; return 0 ;;
  esac
  answer=$(curl -fsS --max-time 10 -H "Accept: application/json" "$gateway/v1/release" 2>/dev/null) || {
    say "version: the latest release — the agents gateway $gateway could not be reached"
    return 0
  }
  flat=$(printf '%s' "$answer" | tr -d '\n\r\t ')
  channel=$(printf '%s' "$flat" | sed -n 's/.*"channel":"\([a-z]*\)".*/\1/p')
  named=$(printf '%s' "$flat" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
  if [ -z "$channel" ]; then
    say "version: the latest release — the agents gateway $gateway sent an answer that is not a release"
  elif [ -z "$named" ]; then
    say "version: the latest release — the agents gateway $gateway (channel $channel) names no version"
  elif ! printf '%s\n' "$named" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    say "version: the latest release — the agents gateway $gateway names \"$named\", which is not a release version"
  else
    version="$named"
    say "version: $version, named by the agents gateway $gateway (channel $channel)"
  fi
}

# no_assets_yet <asset name> is the refusal for a named release whose files are
# not there: a release's assets are built a few minutes after its tag, and a
# gateway names the version the moment it deploys.
no_assets_yet() {
  die "release v$version has no assets yet, or there is no such release ($1 could not be downloaded) — a release's files are published a few minutes after it is tagged; try again in a few minutes, or check the version"
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
      | curl -fsSL -K - "$api/repos/$repo/releases/$rel") || {
      [ -n "$version" ] && no_assets_yet "$1"
      die "the release listing of $repo could not be read (is the token right, is there a release?)"
    }
    # Flatten the JSON, drop the nested objects an asset carries (its
    # uploader), then take the `url` of the object whose `name` is the asset.
    url=$(printf '%s' "$listing" | tr -d '\n ' | sed -E 's/"[A-Za-z_]+":\{[^{}]*\}//g' \
      | sed -n 's/.*"url":"\([^"]*\)"[^{}]*"name":"'"$1"'".*/\1/p')
    if [ -z "$url" ]; then
      [ -n "$version" ] && no_assets_yet "$1"
      die "the release of $repo has no asset named $1"
    fi
    case "$url" in
      "$api"/repos/*) ;;
      *) die "the release listing names an asset outside $api — refusing it" ;;
    esac
    printf 'header = "Authorization: Bearer %s"\nheader = "Accept: application/octet-stream"\n' "$token" \
      | curl -fsSL -K - -o "$2" "$url" \
      || die "downloading $1 failed"
  else
    if [ -n "$version" ]; then dl_path="releases/download/v$version/$1"; else dl_path="releases/latest/download/$1"; fi
    curl -fsSL -o "$2" "$base/$repo/$dl_path" || {
      [ -n "$version" ] && no_assets_yet "$1"
      die "downloading $1 from $repo failed (is there a release, and is the repository public? set UFFICI_GITHUB_TOKEN for a private one)"
    }
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  else die "neither sha256sum nor shasum is available to verify the download" 2
  fi
}

main "$@"
