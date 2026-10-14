#!/bin/bash
# Installs hostinfo on a KVM/libvirt hypervisor (AlmaLinux / RHEL 9 and similar). Run as root from this folder:
#
#   ./install.sh --key "ssh-ed25519 AAAA... pdu-monitor" [--from 10.0.0.5]
#   ./install.sh --update        (what hostinfo-update runs: the key line is never touched)
#   ./install.sh --uninstall
#
# --key   the PUBLIC key of the monitoring system. It can do exactly one thing: run `hostinfo serve` (status / top 1h|6h|24h|7d).
# --from  optional: the address (or comma separated addresses / patterns) the key may be used from.
set -euo pipefail
cd "$(dirname "$0")"

PROGRAM=/usr/local/sbin/hostinfo
UPDATER=/usr/local/sbin/hostinfo-update
DATA=/var/lib/hostinfo
READER=hostinfo-ro

[[ $EUID -eq 0 ]] || { echo "Run as root." >&2; exit 1; }

if [[ "${1:-}" == "--uninstall" ]]; then
  systemctl disable --now hostinfo-collect.timer hostinfo-lvm.timer hostinfo-update.timer 2>/dev/null || true
  rm -f /etc/systemd/system/hostinfo-collect.service /etc/systemd/system/hostinfo-collect.timer \
        /etc/systemd/system/hostinfo-lvm.service /etc/systemd/system/hostinfo-lvm.timer \
        /etc/systemd/system/hostinfo-update.service /etc/systemd/system/hostinfo-update.timer \
        "$PROGRAM" "$UPDATER" "$DATA/installed-commit"
  systemctl daemon-reload
  userdel -r "$READER" 2>/dev/null || true
  userdel hostinfo 2>/dev/null || true
  groupdel hostinfo 2>/dev/null || true
  echo "Removed. The collected data is left in $DATA (delete it by hand if you do not need it)."
  exit 0
fi

KEY=""; FROM=""; UPDATE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --key) KEY="$2"; shift 2 ;;
    --from) FROM="$2"; shift 2 ;;
    --update) UPDATE=1; shift ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

[[ $UPDATE -eq 0 || ( -z "$KEY" && -z "$FROM" ) ]] || { echo "--update keeps the key: it cannot be given with --key or --from." >&2; exit 1; }

command -v python3 >/dev/null || { echo "python3 is needed." >&2; exit 1; }
python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 6) else 1)' || { echo "Python 3.6 or newer is needed." >&2; exit 1; }
python3 -m py_compile hostinfo.py

# two accounts: `hostinfo` collects (writes), `hostinfo-ro` is what the SSH key logs in as (can only read what is collected)
getent group hostinfo >/dev/null || groupadd -r hostinfo
id hostinfo >/dev/null 2>&1 || useradd -r -g hostinfo -d "$DATA" -s /sbin/nologin hostinfo
id "$READER" >/dev/null 2>&1 || useradd -m -s /bin/sh -G hostinfo "$READER"
id -nG "$READER" | tr ' ' '\n' | grep -qx hostinfo || usermod -aG hostinfo "$READER"

install -o root -g root -m 0755 hostinfo.py "$PROGRAM"
install -o root -g root -m 0755 update.sh "$UPDATER"
install -d -o hostinfo -g hostinfo -m 0750 "$DATA"
install -o root -g root -m 0644 systemd/hostinfo-collect.service systemd/hostinfo-collect.timer \
        systemd/hostinfo-lvm.service systemd/hostinfo-lvm.timer \
        systemd/hostinfo-update.service systemd/hostinfo-update.timer /etc/systemd/system/

if [[ -n "$KEY" ]]; then
  [[ "$KEY" =~ ^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp[0-9]+|sk-ssh-ed25519@openssh.com)\  ]] || { echo "--key must be a public key line." >&2; exit 1; }
  [[ "$KEY" != *'"'* && "$FROM" != *'"'* && "$KEY" != *$'\n'* && "$FROM" != *$'\n'* ]] || { echo "No quotes or line breaks in the key or the address." >&2; exit 1; }
  HOME_DIR="$(getent passwd "$READER" | cut -d: -f6)"
  install -d -o "$READER" -g "$READER" -m 0700 "$HOME_DIR/.ssh"
  OPTIONS='restrict,command="'"$PROGRAM"' serve"'
  [[ -z "$FROM" ]] || OPTIONS="$OPTIONS"',from="'"$FROM"'"'
  printf '%s %s\n' "$OPTIONS" "$KEY" > "$HOME_DIR/.ssh/authorized_keys"
  chown "$READER:$READER" "$HOME_DIR/.ssh/authorized_keys"
  chmod 0600 "$HOME_DIR/.ssh/authorized_keys"
fi

if command -v restorecon >/dev/null; then
  restorecon -R "$PROGRAM" "$UPDATER" "$DATA" "$(getent passwd "$READER" | cut -d: -f6)/.ssh" 2>/dev/null || true
fi

systemctl daemon-reload
systemctl enable --now hostinfo-collect.timer
# every night: a new version from GitHub, only if hostagent/ has changed (hostinfo-update --check to look by hand)
systemctl enable --now hostinfo-update.timer
# LVM needs root to be asked: a second, separate timer does only that (every 5 minutes) and leaves one file for the collector
if command -v vgs >/dev/null; then
  systemctl enable --now hostinfo-lvm.timer
  systemctl start hostinfo-lvm.service || echo "  NOTE: hostinfo-lvm.service failed: journalctl -u hostinfo-lvm.service"
else
  systemctl disable --now hostinfo-lvm.timer 2>/dev/null || true
  rm -f "$DATA/lvm.json"
fi

echo
echo "Checks:"
DRY="$(mktemp)"
if runuser -u hostinfo -- "$PROGRAM" collect --dry-run >"$DRY" 2>&1; then
  python3 - "$DRY" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print("  running VMs seen:", d["vms"]["count"])
storage = d.get("storage") or {}
print("  zfs pools seen:  ", [p["name"] + " " + p["health"] for p in storage.get("pools", [])] or "none")
print("  lvm groups seen: ", [g["name"] for g in storage.get("lvm", [])] or "none")
print("  filesystems seen:", [f["mount"] for f in storage.get("fs", [])] or "none")
print("  problems:        ", d["errors"] or "none")
PY
else
  echo "  the dry run failed:"; cat "$DRY"
fi
rm -f "$DRY"
if ! runuser -u hostinfo -- zpool list >/dev/null 2>&1 && command -v zpool >/dev/null; then
  echo "  NOTE: the user 'hostinfo' cannot run zpool here. Let the collector run as root instead:"
  echo "        systemctl edit hostinfo-collect.service   ->   [Service]  User=root  Group=root"
fi
echo
AUTHORIZED="$(getent passwd "$READER" | cut -d: -f6)/.ssh/authorized_keys"
if [[ $UPDATE -eq 1 ]]; then
  echo "Updated to $("$PROGRAM" version). The key line was not touched."
else
  echo "Installed. The first sample is written within a minute: $DATA/status.json"
  if [[ -n "$KEY" ]]; then
    echo "Test from the monitoring system:  ssh -i <private key> $READER@<this host> status"
  elif [[ -s "$AUTHORIZED" ]]; then
    echo "The key line was kept."
  else
    echo "No key given: add one later with: ./install.sh --key \"ssh-ed25519 AAAA...\""
  fi
fi
