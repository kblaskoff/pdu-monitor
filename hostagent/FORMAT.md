# Files and answers of hostinfo (format 1)

Everything is JSON. A consumer must treat it as **untrusted text**: a compromised host can write anything. Cap the size
(a `status` answer is a few kB, a `top 7d` answer under 20 kB), decode strictly, never use a value as a path or a command, and show names as plain text.
Fields may be added in later versions (`v` stays 1 while they are only added); missing numbers are `null`, never 0.

## `hostinfo serve status` (the file /var/lib/hostinfo/status.json, written every minute)

```json
{
  "v": 1, "host": "a52sf", "t": 1760000000, "interval": 60.0,
  "cpu": {"cores": 256, "idle_pct": 61.2, "iowait_pct": 0.1, "load": [96.1, 95.5, 94.0]},
  "mem": {"total": 1081000000000, "available": 480000000000, "used": 601000000000, "swap_used": 0, "zfs_arc": 120000000000},
  "storage": {"t": 1760000000, "zfs": true, "pools": [
     {"name": "pool", "size": 91534343012352, "alloc": 59918395113472, "free": 31615947898880, "cap_pct": 65, "health": "ONLINE",
      "usable_used": 53246036254320, "usable_avail": 27960521023888, "usable_total": 81206557278208}]},
  "vms": {"count": 12, "list": [{"name": "altar22", "pid": 4321, "rss": 8589934592, "cores": 1.25}]},
  "other_cores": 0.4,
  "agent": {"version": "0.1.0", "python": "3.9.25"},
  "errors": []
}
```

* `t` is the time of the sample (unix seconds); the consumer must show how old it is. Older than about 3 intervals = stale.
* `cpu.idle_pct`, `iowait_pct`, `vms.list[].cores` and `other_cores` are averages **since the previous sample** (`null` on the first one).
  `cores` is how many cores worth of CPU the VM used (1.25 = a core and a quarter), counted over all its threads.
* `mem.used` = total - available. On a ZFS host the ARC (`zfs_arc`) is part of "used" and is given apart, so a screen can show it.
* `storage.pools[].size/alloc/free` are raw (a raidz1 pool counts its parity); `usable_*` is what the datasets can really hold. `health` is
  ONLINE / DEGRADED / FAULTED ...: anything but ONLINE needs attention. The pools are asked every 5 minutes (`storage.t`).
* `errors` lists what could not be read (a VM, ZFS); the rest of the sample is still valid.

## `hostinfo serve top 1h|6h|24h|7d`

```json
{"v": 1, "host": "a52sf", "window": "6h", "t": 1760000000, "cores": 256, "covered_s": 21540.0, "coverage": 0.998, "samples": 359,
 "other_cpu_s": 5400.2,
 "vms": [{"name": "altar22", "cpu_s": 51234.5, "avg_cores": 2.38, "avg_pct": 0.93, "peak_cores": 8.1, "peak_t": 1759990000}]}
```

All VMs seen in the window, busiest first (the screen takes the first 5). `cpu_s` is the CPU time used in the window, `avg_cores` = cpu_s / covered_s,
`avg_pct` is that as a share of the whole host, `peak_*` is the busiest single interval. `coverage` is how much of the window has samples
(below 1 while the agent is young or was stopped): show it. `other_cpu_s` is CPU used by the host itself (not by a VM).

## Errors

`{"error": "command not allowed"}` exit code 2 (anything other than the five commands), `{"error": "no data: ..."}` exit code 1.

## On disk (/var/lib/hostinfo, owner hostinfo, group hostinfo, mode 0750)

| file | content |
| --- | --- |
| `status.json` | the last sample (0640) |
| `samples-YYYYMMDD.ndjson` | one line per sample: `{"t":…,"d":60.0,"c":{"vm":cpu_seconds,…},"o":other_cpu_seconds,"i":idle_pct}`, UTC days, kept 8 days (about 0.7 MB a day with 20 VMs) |
| `state.json` | the counters of the previous sample (0600) |
