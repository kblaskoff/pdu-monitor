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

```
./install.sh --key "ssh-ed25519 AAAA… pdu-monitor" --from 10.0.0.5
```

`--key` is the **public** key of the monitoring system (make a separate one: `ssh-keygen -t ed25519 -f hostinfo_key`). `--from` limits
where it may log in from. The script creates the two accounts, installs `/usr/local/sbin/hostinfo`, the timer and the key line

```
restrict,command="/usr/local/sbin/hostinfo serve",from="10.0.0.5" ssh-ed25519 AAAA… pdu-monitor
```

so the key cannot open a shell, forward ports or run anything but `hostinfo serve`, which answers exactly: `status`, `top 1h`, `top 6h`,
`top 24h`, `top 7d`, `version` (nothing else is accepted, and nothing of the command is given to a shell).

Check on the host after a minute:  `cat /var/lib/hostinfo/status.json`  and  `systemctl list-timers hostinfo-collect.timer`.
Check from the monitoring system:  `ssh -i hostinfo_key hostinfo-ro@HOST status`  and  `ssh -i hostinfo_key hostinfo-ro@HOST "top 6h"`.
A dry run that writes nothing:  `sudo -u hostinfo /usr/local/sbin/hostinfo collect --dry-run`.
Remove everything:  `./install.sh --uninstall`.

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
