#!/usr/bin/env bash
# Installs the PDU Monitor server on Ubuntu 22.04 (any systemd Linux).
#   sudo ./install.sh [--binary ./pdumonitord]
# Safe to run again: the configuration, the data and the keys are never overwritten.
set -euo pipefail

BIN=./pdumonitord
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BIN=$2; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
[ -x "$BIN" ] || { echo "binary not found: $BIN" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)

id pdumonitor >/dev/null 2>&1 || useradd --system --home-dir /var/lib/pdu-monitor --shell /usr/sbin/nologin pdumonitor
install -d -m 750 -o pdumonitor -g pdumonitor /var/lib/pdu-monitor
install -d -m 750 -o root -g pdumonitor /etc/pdu-monitor
install -m 755 "$BIN" /usr/local/bin/pdumonitord
install -m 644 "$HERE/pdu-monitor.service" /etc/systemd/system/pdu-monitor.service

# the "Update" button of the web interface: the web process leaves a file, these (root) units do the work
install -m 755 "$HERE/update.sh" /usr/local/sbin/pdu-update
install -m 644 "$HERE/pdu-monitor-update.path" "$HERE/pdu-monitor-update.service" /etc/systemd/system/
if [ ! -e /etc/pdu-monitor/update.conf ]; then
  REPO=$(git -c safe.directory='*' -C "$HERE" rev-parse --show-toplevel 2>/dev/null || true)
  printf '# the git clone the update button builds from (git fetch + fast-forward + go build)\nREPO_DIR=%s\nBRANCH=main\n' "$REPO" > /etc/pdu-monitor/update.conf
  chmod 644 /etc/pdu-monitor/update.conf
  [ -n "$REPO" ] || echo "NOTE: put the path of your git clone into REPO_DIR in /etc/pdu-monitor/update.conf (the update button needs it)."
fi
touch /var/lib/pdu-monitor/.updater && chown pdumonitor:pdumonitor /var/lib/pdu-monitor/.updater

if [ ! -e /etc/pdu-monitor/config.yaml ]; then
  /usr/local/bin/pdumonitord init -config /etc/pdu-monitor/config.yaml
  chown root:pdumonitor /etc/pdu-monitor/config.yaml
  chmod 640 /etc/pdu-monitor/config.yaml
  echo "Wrote /etc/pdu-monitor/config.yaml: edit it (address, certificate, LDAP) before starting."
fi

systemctl daemon-reload
systemctl enable pdu-monitor.service >/dev/null
systemctl enable --now pdu-monitor-update.path >/dev/null
echo
echo "Installed. Next:"
echo "  1. sudo nano /etc/pdu-monitor/config.yaml"
echo "  2. certificate:  sudo pdumonitord selfsigned -host <name> -out /etc/pdu-monitor && sudo chgrp pdumonitor /etc/pdu-monitor/tls.key && sudo chmod 640 /etc/pdu-monitor/tls.key"
echo "                   (or the certificate and key from your own CA, readable by the group pdumonitor)"
echo "  3. directory:    sudo -u pdumonitor pdumonitord ldap-test -config /etc/pdu-monitor/config.yaml -user <your login>"
echo "  4. start:        sudo systemctl start pdu-monitor && journalctl -u pdu-monitor -f"
echo "  5. host agents:  sudo -u pdumonitor pdumonitord pubkey -config /etc/pdu-monitor/config.yaml"
