#!/usr/bin/env bash
# Updates the PDU Monitor server or the collector of this machine from its git clone. It runs as root, started by systemd when the
# application leaves an update-request file in its data directory (the "Update" button); the web process itself runs nothing.
#   pdu-update server|collector
# What it does: fetch the branch, fast-forward the clone, build into a temporary directory, stop if the build is the same commit as the
# installed one, otherwise replace the binary, restart, and go back to the old binary if the new one does not stay up.
# Settings: /etc/pdu-monitor/update.conf or /etc/pdu-collector/update.conf (REPO_DIR=..., BRANCH=main).
set -uo pipefail

KIND=${1:-}
case "$KIND" in
  server)    NAME=pdu-monitor;   BIN_NAME=pdumonitord;  PKG=./cmd/pdumonitord;  CONF_DEFAULT=/etc/pdu-monitor/update.conf;   STATE_DEFAULT=/var/lib/pdu-monitor ;;
  collector) NAME=pdu-collector; BIN_NAME=pdu-collector; PKG=./cmd/pdu-collector; CONF_DEFAULT=/etc/pdu-collector/update.conf; STATE_DEFAULT=/var/lib/pdu-collector ;;
  *) echo "usage: $0 server|collector" >&2; exit 2 ;;
esac
# the defaults are for a real machine; the tests of this script point them at a scratch directory
CONF=${UPDATE_CONF:-$CONF_DEFAULT}
STATE=${STATE_DIR:-$STATE_DEFAULT}
BIN_DIR=${BIN_DIR:-/usr/local/bin}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
SETTLE=${SETTLE:-15}                 # seconds the new version has to stay up
export HOME=${HOME:-/root}
export PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin:/usr/local/go/bin:/usr/lib/go/bin:/usr/local/bin:/snap/bin"   # systemd services do not have /snap/bin (Go from snap)

status() { printf '%s\t%s\t%s\n' "$1" "$(date +%s)" "$2" > "$STATE/update.status.tmp" && mv -f "$STATE/update.status.tmp" "$STATE/update.status"; chmod 644 "$STATE/update.status"; }
fail() { echo "FAILED: $1"; status failed "$1"; exit 1; }

# the request goes first: one that arrives while this runs starts another run afterwards
rm -f "$STATE/update-request"
: > "$STATE/update.log"; chmod 644 "$STATE/update.log"
exec >> "$STATE/update.log" 2>&1
echo "== $(date -u '+%Y-%m-%d %H:%M:%S UTC') update of the $KIND"
status running "started"

REPO_DIR=""; BRANCH=main
# shellcheck disable=SC1090
[ -r "$CONF" ] && . "$CONF"
[ -n "$REPO_DIR" ] && [ -d "$REPO_DIR/.git" ] || fail "REPO_DIR in $CONF is not a git clone of the repository"
command -v go >/dev/null || fail "go is not installed on this machine"
# git runs as the owner of the clone, so that the files it updates stay his (a root-owned update would break his next git pull);
# the build runs as root, and the git that go starts for the version stamp must accept a clone of another owner
OWNER=$(stat -c %U "$REPO_DIR" 2>/dev/null || echo root)
if [ "$OWNER" = root ]; then
  GIT=(git -C "$REPO_DIR")
else
  OWNER_HOME=$(getent passwd "$OWNER" | cut -d: -f6)
  GIT=(runuser -u "$OWNER" -- env "HOME=${OWNER_HOME:-/}" git -C "$REPO_DIR")
fi
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'

status running "fetching $BRANCH"
"${GIT[@]}" fetch --quiet origin "$BRANCH" || fail "git fetch of $BRANCH failed (network, or the clone has no origin)"
"${GIT[@]}" merge --ff-only FETCH_HEAD || fail "the clone cannot be fast-forwarded to origin/$BRANCH (local changes or another branch): fix $REPO_DIR by hand"

TMP=$(mktemp -d) || fail "no temporary directory"
trap 'rm -rf "$TMP"' EXIT
status running "building"
( cd "$REPO_DIR/server" && CGO_ENABLED=0 go build -o "$TMP/$BIN_NAME" "$PKG" ) || fail "go build failed (the output is above)"
NEW=$("$TMP/$BIN_NAME" version) || fail "the new binary does not run"
OLD=$("$BIN_DIR/$BIN_NAME" version 2>/dev/null || echo none)
echo "installed: $OLD   new: $NEW"
if [ "$NEW" = "$OLD" ]; then
  status uptodate "already the latest ($NEW)"
  exit 0
fi

status running "installing"
[ -e "$BIN_DIR/$BIN_NAME" ] && cp -p "$BIN_DIR/$BIN_NAME" "$BIN_DIR/$BIN_NAME.prev"
install -m 755 "$TMP/$BIN_NAME" "$BIN_DIR/$BIN_NAME.new" && mv -f "$BIN_DIR/$BIN_NAME.new" "$BIN_DIR/$BIN_NAME" || fail "the binary could not be installed"
status running "restarting"
"$SYSTEMCTL" restart "$NAME.service"
sleep "$SETTLE"
RESTARTS=$("$SYSTEMCTL" show -p NRestarts --value "$NAME.service" 2>/dev/null || echo 0)
if "$SYSTEMCTL" is-active --quiet "$NAME.service" && [ "${RESTARTS:-0}" = 0 ]; then
  echo "running: $NEW"
  status ok "$OLD -> $NEW"
  exit 0
fi
echo "the new version did not stay up (restarts: ${RESTARTS:-?})"
"$SYSTEMCTL" status --no-pager -n 20 "$NAME.service" || true
if [ -e "$BIN_DIR/$BIN_NAME.prev" ]; then
  mv -f "$BIN_DIR/$BIN_NAME.prev" "$BIN_DIR/$BIN_NAME"
  "$SYSTEMCTL" restart "$NAME.service"
  fail "the new version ($NEW) did not stay up: the old one ($OLD) is back"
fi
fail "the new version ($NEW) did not stay up and there is no old one to go back to"
