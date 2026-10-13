# hostinfo – host agent for the hypervisors

A small program for each KVM/libvirt hypervisor. Every minute it notes how loaded the host is and **which VM uses the CPU**, and keeps
the history of 7 days in plain files. The monitoring system reads it over SSH with a key that can do nothing else.

* **No network service.** Nothing listens. A systemd timer starts `hostinfo collect` once a minute; it takes about 40 ms and 10 MB of memory
  (measured: 40 ms of CPU for a sample, 5 MB for 7 days of 20 VMs, 0.16 s to answer a 7 day question).
* **No root.** It runs as the account `hostinfo`, which only reads `/proc`, `/run/libvirt/qemu/*.pid` and asks `zpool`/`zfs`.
  The SSH key logs in as a second account, `hostinfo-ro`, which can only read the collected files.
* **No database, no dependencies.** Python 3.9 standard library only (AlmaLinux 9 has it). Files are described in [FORMAT.md](FORMAT.md).
* **Exact CPU numbers.** The CPU time of a VM is the counter of its qemu process; the difference of two readings is what it used in between.
  A short peak is not lost, and the top 5 of 1 h / 6 h / 24 h / 7 d are sums of real CPU time, not guesses from snapshots.

What it shows: CPU idle % and load, cores, memory (used/available, ZFS ARC apart), ZFS pools (raw and usable space, health: a DEGRADED raidz1 is
a warning), number of running VMs, CPU of every VM, the host's own CPU. It sees the VMs from outside: *which VM* loads the host, not which process inside a VM.

## Install (on each hypervisor, as root)

The agent needs Python 3.9 (AlmaLinux / RHEL 9 have it) and nothing else. It is read by `pdumonitord` itself, or by a
`pdu-collector` where the server cannot reach the hypervisors; the steps are the same, only the key differs. In the examples,
`SERVER-IP` is the address the server (or the collector) connects from and `HYPERVISOR` is the host.

**1. The key of the server.** On the server:

```
sudo -u pdumonitor pdumonitord pubkey -config /etc/pdu-monitor/config.yaml
```

It prints one line, `ssh-ed25519 AAAA… pdu-monitor` (the same as Settings → Hosts in the web interface). For a collector use
`sudo -u pdu-collector pdu-collector pubkey -config /etc/pdu-collector/config.yaml` instead.

**2. The files.** On the hypervisor, take this folder from the archive of `main` on GitHub (needs only `curl` and `tar`, no git):

```
mkdir -p /opt/pdu-monitor
curl -fsSL https://github.com/kblaskoff/pdu-monitor/archive/refs/heads/main.tar.gz | tar xz -C /opt/pdu-monitor --strip-components=1 pdu-monitor-main/hostagent
```

**3. Install.** On the hypervisor, as root:

```
cd /opt/pdu-monitor/hostagent
./install.sh --key "ssh-ed25519 AAAA… pdu-monitor" --from SERVER-IP
```

The script creates the accounts `hostinfo` (collects) and `hostinfo-ro` (what the key logs in as), installs `/usr/local/sbin/hostinfo`,
`/var/lib/hostinfo` and the timer, writes the key line and ends with a dry run: the number of running VMs, the ZFS pools and any problem.
`--from` limits where the key may be used from (several addresses: `--from 10.0.0.5,10.0.0.6`). The key line is

```
restrict,command="/usr/local/sbin/hostinfo serve",from="SERVER-IP" ssh-ed25519 AAAA… pdu-monitor
```

so the key cannot open a shell, forward ports or run anything but `hostinfo serve`, which answers exactly: `status`, `top 1h`, `top 6h`,
`top 24h`, `top 7d`, `version` (nothing else is accepted, and nothing of the command is given to a shell). There is one key per host:
`--key` replaces the one that was there.

**4. Check on the hypervisor** after a minute:

```
cat /var/lib/hostinfo/status.json
systemctl list-timers hostinfo-collect.timer
sudo -u hostinfo /usr/local/sbin/hostinfo collect --dry-run      # a sample that writes nothing
```

**5. Add the host in the web interface.** Settings → Hosts → **Add host**: name, address, SSH user `hostinfo-ro`, and the rack and power
outlet name (`200U31`) that join it with its power. Press **Check the key**, compare the fingerprint with the one the hypervisor prints:

```
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

and paste it to confirm. Nothing is read before that. A collector does the same with `pdu-collector trust`, and its hosts appear in
Settings → Hosts by themselves.

**6. Check from the server** (optional; ssh shows the host key and asks before it connects, compare it as in step 5):

```
sudo -u pdumonitor ssh -i /var/lib/pdu-monitor/ssh_ed25519 -o UserKnownHostsFile=/dev/null hostinfo-ro@HYPERVISOR status
sudo -u pdumonitor ssh -i /var/lib/pdu-monitor/ssh_ed25519 -o UserKnownHostsFile=/dev/null hostinfo-ro@HYPERVISOR "top 6h"
```

**Several hypervisors** from the server in one go (each still needs step 5):

```
KEY="$(sudo -u pdumonitor pdumonitord pubkey -config /etc/pdu-monitor/config.yaml)"
for h in HYPERVISOR1 HYPERVISOR2; do
  ssh root@$h "mkdir -p /opt/pdu-monitor && curl -fsSL https://github.com/kblaskoff/pdu-monitor/archive/refs/heads/main.tar.gz | tar xz -C /opt/pdu-monitor --strip-components=1 pdu-monitor-main/hostagent && cd /opt/pdu-monitor/hostagent && ./install.sh --key '$KEY' --from SERVER-IP"
done
```

## Update

Take the files again (the same line as in step 2; it replaces them in place) and run the script **without** `--key`:

```
curl -fsSL https://github.com/kblaskoff/pdu-monitor/archive/refs/heads/main.tar.gz | tar xz -C /opt/pdu-monitor --strip-components=1 pdu-monitor-main/hostagent
cd /opt/pdu-monitor/hostagent && ./install.sh
/usr/local/sbin/hostinfo version
```

`/opt/pdu-monitor` is only where the files are installed from: the timer runs `/usr/local/sbin/hostinfo` and the data is in
`/var/lib/hostinfo`, so nothing that is running reads the folder while it is replaced.

Without `--key` the key line stays as it is. The collected history in `/var/lib/hostinfo` and changes made with `systemctl edit`
(for example `User=root`) stay too. Nothing has to be done on the server: it reads the new version at its next poll.
For all hypervisors from the server: the loop above with `./install.sh` instead of `./install.sh --key …`.

## Remove

`./install.sh --uninstall` removes the program, the timer and both accounts; the collected data is left in `/var/lib/hostinfo`.
Delete the host in Settings → Hosts as well.

## If something does not work

* **No VMs listed, `errors` mentions `/proc`:** `/proc` may be mounted with `hidepid`, so a normal account cannot see the qemu processes.
  Run the collector as root: `systemctl edit hostinfo-collect.service` → `[Service]` `User=root` `Group=root`.
* **No storage although there is a pool:** the account `hostinfo` could not run `zpool` (the install script warns about it). Same remedy as above.
* **SELinux:** `ausearch -m avc -ts recent`; the install script runs `restorecon`. The program is in `/usr/local/sbin` (type `bin_t`) on purpose.
* **ssh asks for a password:** the key line is in `~hostinfo-ro/.ssh/authorized_keys`, the home and `.ssh` must not be writable by others; check `journalctl -u sshd`.
* **Different timing:** change `OnUnitActiveSec` in the timer (and set `Environment=HOSTINFO_INTERVAL=<seconds>` in the service so that gaps are recognized).

## Tests

`python3 -m unittest discover -s tests` (Python 3.9 or newer, Linux). They use a fake `/proc` and libvirt folder, a mutation of the code is caught
by them (checked by hand), and one test reads the real `/proc` of the machine. The install script and the systemd units are checked on a real host only.
