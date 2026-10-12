#!/usr/bin/env bash
# Installs the PDU Monitor collector (any systemd Linux).
#   sudo ./install.sh [--binary ./pdu-collector]
# Safe to run again: the configuration, the token and the keys are never overwritten.
set -euo pipefail

BIN=./pdu-collector
while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BIN=$2; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
[ -x "$BIN" ] || { echo "binary not found: $BIN" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)

id pdu-collector >/dev/null 2>&1 || useradd --system --home-dir /var/lib/pdu-collector --shell /usr/sbin/nologin pdu-collector
install -d -m 750 -o pdu-collector -g pdu-collector /var/lib/pdu-collector
install -d -m 750 -o root -g pdu-collector /etc/pdu-collector
install -m 755 "$BIN" /usr/local/bin/pdu-collector
install -m 644 "$HERE/pdu-collector.service" /etc/systemd/system/pdu-collector.service

if [ ! -e /etc/pdu-collector/config.yaml ]; then
  /usr/local/bin/pdu-collector init -config /etc/pdu-collector/config.yaml
  chown root:pdu-collector /etc/pdu-collector/config.yaml
  chmod 640 /etc/pdu-collector/config.yaml
fi

systemctl daemon-reload
systemctl enable pdu-collector.service >/dev/null
echo
echo "Installed. Next:"
echo "  1. On the server:  pdumonitord collector-token -name collector-a   (put the lines it prints into the server's config.yaml, restart the server)"
echo "  2. Here:           echo 'THE-TOKEN' | sudo tee /etc/pdu-collector/token >/dev/null && sudo chown root:pdu-collector /etc/pdu-collector/token && sudo chmod 640 /etc/pdu-collector/token"
echo "  3. sudo nano /etc/pdu-collector/config.yaml   (server address and the hypervisors)"
echo "  4. The key to give the agents:   sudo -u pdu-collector pdu-collector pubkey -config /etc/pdu-collector/config.yaml"
echo "     (on each hypervisor: ./install.sh --key 'THE-KEY')"
echo "  5. Confirm each host's key:      sudo -u pdu-collector pdu-collector trust -config /etc/pdu-collector/config.yaml -all"
echo "  6. Try once:                     sudo -u pdu-collector pdu-collector once -config /etc/pdu-collector/config.yaml"
echo "  7. sudo systemctl start pdu-collector && journalctl -u pdu-collector -f"
