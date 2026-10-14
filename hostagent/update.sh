#!/bin/bash
# hostinfo-update: installs a new version of hostinfo from GitHub, but only when something in hostagent/ has changed.
# Installed as /usr/local/sbin/hostinfo-update; a timer runs it every night, and it can be run by hand (as root):
#
#   hostinfo-update            install the new version, if there is one
#   hostinfo-update --check    only say whether there is one (exit 0: up to date, 10: a new version is there)
#   hostinfo-update --force    install the newest version even if it is the one installed
#   --quiet                    print nothing when there is nothing to do (for the timer)
#
# GitHub is asked for the last commit that changed hostagent/ (a 4 KB answer; nothing is downloaded when it is the one
# installed). A new version is the archive of exactly that commit; its install.sh runs with --update, which keeps the key,
# the history and the `systemctl edit` changes. If any step fails, the installed version stays and the next run tries again.
set -euo pipefail

REPO="${HOSTINFO_REPO:-kblaskoff/pdu-monitor}"
REF="${HOSTINFO_REF:-main}"
STATE="${HOSTINFO_UPDATE_STATE:-/var/lib/hostinfo/installed-commit}"
LOCK="${HOSTINFO_UPDATE_LOCK:-/run/hostinfo-update.lock}"
PROGRAM="${HOSTINFO_PROGRAM:-/usr/local/sbin/hostinfo}"

# Everything is in a function that is called on the last line: bash reads a script while it runs it, and install.sh replaces
# this very file. A function is read completely before it starts.
main() {
  local check=0 force=0 quiet=0
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --check) check=1 ;;
      --force) force=1 ;;
      --quiet) quiet=1 ;;
      *) echo "Unknown option: $1 (use --check, --force, --quiet)" >&2; return 2 ;;
    esac
    shift
  done
  [[ "$REPO" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ && "$REF" =~ ^[A-Za-z0-9][A-Za-z0-9_./-]*$ && "$REPO$REF" != *..* ]] \
    || { echo "HOSTINFO_REPO or HOSTINFO_REF is not valid." >&2; return 2; }
  [[ $EUID -eq 0 || -n "${HOSTINFO_UPDATE_STATE:-}" ]] || { echo "Run as root." >&2; return 1; }

  exec 9>"$LOCK"
  flock -n 9 || { echo "Another hostinfo-update is running." >&2; return 1; }

  local answer latest installed
  answer="$(curl -fsS --max-time 30 -H 'Accept: application/vnd.github+json' \
            "https://api.github.com/repos/$REPO/commits?sha=$REF&path=hostagent&per_page=1")" \
    || { echo "GitHub did not answer: nothing was changed." >&2; return 1; }
  latest="$(printf '%s' "$answer" | python3 -c 'import json, sys; print(json.load(sys.stdin)[0]["sha"])' 2>/dev/null)" || latest=""
  [[ "$latest" =~ ^[0-9a-f]{40}$ ]] || { echo "GitHub gave no commit for hostagent/ on $REF: nothing was changed." >&2; return 1; }
  installed="$(cat "$STATE" 2>/dev/null || true)"

  if [[ "$latest" == "$installed" && $force -eq 0 ]]; then
    [[ $quiet -eq 1 ]] || echo "Up to date: $("$PROGRAM" version 2>/dev/null || echo '?') (commit ${latest:0:7})."
    return 0
  fi
  if [[ $check -eq 1 ]]; then
    local shown="${installed:0:7}"
    echo "A new version is there: commit ${latest:0:7} (installed: ${shown:-unknown})."
    return 10
  fi

  local tmp old new
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  curl -fsSL --max-time 300 -o "$tmp/archive.tar.gz" "https://github.com/$REPO/archive/$latest.tar.gz" \
    || { echo "The download failed: nothing was changed." >&2; return 1; }
  tar xzf "$tmp/archive.tar.gz" -C "$tmp" --strip-components=1 "${REPO#*/}-$latest/hostagent" 2>/dev/null \
    && [[ -f "$tmp/hostagent/install.sh" && -f "$tmp/hostagent/hostinfo.py" ]] \
    || { echo "The archive of ${latest:0:7} has no hostagent folder: nothing was changed." >&2; return 1; }
  python3 -m py_compile "$tmp/hostagent/hostinfo.py" 2>/dev/null \
    || { echo "hostinfo.py of ${latest:0:7} does not compile: nothing was changed." >&2; return 1; }

  old="$("$PROGRAM" version 2>/dev/null || echo none)"
  if ! (cd "$tmp/hostagent" && bash ./install.sh --update) >"$tmp/install.log" 2>&1; then
    echo "install.sh of ${latest:0:7} failed (the next run tries again):" >&2
    cat "$tmp/install.log" >&2
    return 1
  fi
  printf '%s\n' "$latest" > "$STATE.new" && mv -f "$STATE.new" "$STATE"
  new="$("$PROGRAM" version 2>/dev/null || echo '?')"
  echo "Updated hostinfo $old -> $new (commit ${latest:0:7})."
  [[ $quiet -eq 1 ]] || cat "$tmp/install.log"
}

main "$@"
exit $?
