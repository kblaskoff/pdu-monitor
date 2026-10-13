#!/usr/bin/python3
"""hostinfo - light health and VM load sampler for a KVM/libvirt hypervisor.

Four commands, no dependencies beyond the Python standard library (3.6 or newer):

  hostinfo collect [--dry-run]   one sample, run by a systemd timer (every 60 s). Reads /proc and the libvirt pid files, calls
                                 `zpool`/`zfs` at most every 5 minutes, writes status.json and appends one line to the day's
                                 samples file. No network, no listening socket, no input from anybody.
  hostinfo lvm-collect           run as root by its own timer (every 5 minutes) on a host with LVM: asks `vgs` and `lvs` (they need
                                 root) and writes lvm.json for `collect` to pick up. Fixed commands, no input from anybody.
  hostinfo serve [command]       what the SSH key of the monitoring system may run (as a forced command). The command comes from
                                 SSH_ORIGINAL_COMMAND and must be exactly one of: status | top 1h | top 6h | top 24h | top 7d | version.
  hostinfo version

The CPU time of a VM is read from the counters of its qemu process (/proc/<pid>/stat), not from a snapshot of `top`: the difference of
two readings is the exact CPU time the VM used between them, so a short peak is not missed and the totals for 1 h .. 7 d are exact.
The format of the files is described in FORMAT.md.
"""
import json
import os
import re
import subprocess
import sys
import time

VERSION = "0.2.0"
FORMAT = 1

WINDOWS = {"1h": 3600, "6h": 6 * 3600, "24h": 24 * 3600, "7d": 7 * 24 * 3600}
RETENTION_DAYS = 8
STORAGE_REFRESH = 300          # seconds: the pools change slowly, they are asked every 5 minutes only
LVM_FILE = "lvm.json"          # written by `lvm-collect` (root), read by `collect`
LVM_MAX_AGE = 1200             # seconds: a lvm.json older than this is reported as a problem, not shown as if it were current
FS_TYPES = ("ext2", "ext3", "ext4", "xfs", "btrfs", "f2fs", "jfs", "reiserfs")   # local filesystems that are listed
MAX_FILESYSTEMS = 30
MAX_VGS = 20
STAT_FIELDS = 8                # user nice system idle iowait irq softirq steal (guest time is already inside user and nice)
SAFE_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"


class Config(object):
    """Where things are. The defaults are for a real host; the tests point them at a fake tree."""

    def __init__(self, data_dir=None, proc=None, libvirt=None, interval=None, clk_tck=None, run=None, statvfs=None):
        self.data_dir = data_dir or os.environ.get("HOSTINFO_DIR", "/var/lib/hostinfo")
        self.proc = proc or os.environ.get("HOSTINFO_PROC", "/proc")
        self.libvirt = libvirt or os.environ.get("HOSTINFO_LIBVIRT", "/run/libvirt/qemu")
        self.interval = float(interval or os.environ.get("HOSTINFO_INTERVAL", "60"))
        self.clk_tck = clk_tck or os.sysconf("SC_CLK_TCK")
        self.run = run or run_command
        self.statvfs = statvfs or os.statvfs

    @property
    def gap_limit(self):
        # After a longer pause (collector stopped, host suspended) a reading would describe a long time, not an interval: skip it.
        return self.interval * 5 + 30


def run_command(argv):
    """Runs a command with a fixed PATH, no shell, a time limit. Returns its output, or None when it is not installed or fails."""
    try:
        result = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15,
                                env={"PATH": SAFE_PATH, "LC_ALL": "C"}, universal_newlines=True)
    except (OSError, subprocess.SubprocessError):
        return None
    return result.stdout if result.returncode == 0 else None


# ----------------------------------------------------------------------------------------------------------------- readers

def read_text(path):
    with open(path, "r") as handle:
        return handle.read()


def read_cpu_stat(cfg):
    """Returns (counters of the 'cpu' line, number of cores)."""
    first = None
    cores = 0
    for line in read_text(os.path.join(cfg.proc, "stat")).splitlines():
        if line.startswith("cpu "):
            first = [int(x) for x in line.split()[1:]]
        elif line.startswith("cpu") and line[3:4].isdigit():
            cores += 1
    if first is None or len(first) < STAT_FIELDS:
        raise ValueError("no cpu line in /proc/stat")
    return first[:STAT_FIELDS], max(cores, 1)


def read_loadavg(cfg):
    parts = read_text(os.path.join(cfg.proc, "loadavg")).split()
    return [float(parts[0]), float(parts[1]), float(parts[2])]


def read_meminfo(cfg):
    values = {}
    for line in read_text(os.path.join(cfg.proc, "meminfo")).splitlines():
        key, _, rest = line.partition(":")
        fields = rest.split()
        if fields:
            values[key] = int(fields[0]) * 1024            # the file is in kB
    total = values.get("MemTotal", 0)
    available = values.get("MemAvailable", values.get("MemFree", 0))
    swap_used = values.get("SwapTotal", 0) - values.get("SwapFree", 0)
    return {"total": total, "available": available, "used": max(total - available, 0), "swap_used": max(swap_used, 0)}


def read_zfs_arc(cfg):
    """Size of the ZFS ARC in bytes (it counts as 'used' memory, so the screen can tell it apart), or None without ZFS."""
    try:
        for line in read_text(os.path.join(cfg.proc, "spl/kstat/zfs/arcstats")).splitlines():
            parts = line.split()
            if len(parts) == 3 and parts[0] == "size":
                return int(parts[2])
    except (OSError, ValueError):
        pass
    return None


def clean_name(name):
    """The names of VMs come from file names: control characters are removed and the length is limited."""
    return "".join(ch for ch in name if ch >= " " and ch != "\x7f")[:100]


def list_vms(cfg):
    """name -> pid of the running VMs, from the pid files of libvirt."""
    result = {}
    try:
        names = os.listdir(cfg.libvirt)
    except OSError:
        return result
    for entry in sorted(names):
        if not entry.endswith(".pid"):
            continue
        try:
            pid = int(read_text(os.path.join(cfg.libvirt, entry)).strip())
        except (OSError, ValueError):
            continue
        if pid > 0:
            result[clean_name(entry[:-4])] = pid
    return result


def read_process(cfg, pid):
    """(cpu ticks, start time, resident bytes) of a process. The counters of all its threads (vcpus, io threads) are in it."""
    data = read_text(os.path.join(cfg.proc, str(pid), "stat"))
    rest = data[data.rfind(")") + 2:].split()          # the name of the process may contain spaces and brackets: skip it
    ticks = int(rest[11]) + int(rest[12])
    start = int(rest[19])
    rss = 0
    try:
        rss = int(read_text(os.path.join(cfg.proc, str(pid), "statm")).split()[1]) * os.sysconf("SC_PAGE_SIZE")
    except (OSError, ValueError, IndexError):
        pass
    return ticks, start, rss


def is_qemu(cfg, pid):
    """A pid file can outlive its VM and the number can be reused: a process that is clearly something else is not counted."""
    try:
        comm = read_text(os.path.join(cfg.proc, str(pid), "comm")).strip().lower()
    except OSError:
        return True            # no way to tell: the stat file decides
    return ("qemu" in comm) or ("kvm" in comm)


def parse_zfs(zpool_out, zfs_out):
    """`zpool list -Hp -o name,size,alloc,free,cap,health` and `zfs list -Hp -d 0 -o name,used,avail` -> list of pools.
    size/alloc/free are raw (they include the parity of raidz); usable_* are what the datasets can really hold."""
    usable = {}
    for line in (zfs_out or "").splitlines():
        parts = line.split("\t")
        if len(parts) >= 3:
            try:
                usable[parts[0]] = (int(parts[1]), int(parts[2]))
            except ValueError:
                pass
    pools = []
    for line in (zpool_out or "").splitlines():
        parts = line.split("\t")
        if len(parts) < 6:
            continue
        try:
            pool = {"name": clean_name(parts[0]), "size": int(parts[1]), "alloc": int(parts[2]), "free": int(parts[3]),
                    "cap_pct": int(parts[4]), "health": clean_name(parts[5])}
        except ValueError:
            continue
        if parts[0] in usable:
            used, avail = usable[parts[0]]
            pool["usable_used"], pool["usable_avail"], pool["usable_total"] = used, avail, used + avail
        pools.append(pool)
    return pools


def read_storage(cfg):
    zpool = cfg.run(["zpool", "list", "-Hp", "-o", "name,size,alloc,free,cap,health"])
    if zpool is None:
        return {"pools": [], "zfs": False}
    zfs = cfg.run(["zfs", "list", "-Hp", "-d", "0", "-o", "name,used,avail"])
    return {"pools": parse_zfs(zpool, zfs), "zfs": True}


def unescape_mount(text):
    """/proc/mounts writes a space (and a few other characters) as \\040 and so on."""
    return re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), text)


def read_filesystems(cfg):
    """Size and use of the local filesystems (ext4, xfs, ...) that are mounted: the root filesystem filling up stops a host as surely as
    a full pool. A device mounted twice (bind mounts) is listed once. Needs no privileges: statvfs works on any mount point."""
    try:
        lines = read_text(os.path.join(cfg.proc, "mounts")).splitlines()
    except OSError:
        return []
    seen = set()
    result = []
    for line in lines:
        parts = line.split()
        if len(parts) < 3 or parts[2] not in FS_TYPES:
            continue
        device, mount = unescape_mount(parts[0]), unescape_mount(parts[1])
        if device in seen:
            continue
        seen.add(device)
        try:
            info = cfg.statvfs(mount)
        except OSError:
            continue                               # not visible to this user or gone: not a reason to stop the sample
        size = info.f_blocks * info.f_frsize
        if size <= 0:
            continue
        result.append({"mount": clean_name(mount), "dev": clean_name(device), "type": parts[2], "size": size,
                       "used": (info.f_blocks - info.f_bfree) * info.f_frsize, "avail": info.f_bavail * info.f_frsize})
    result.sort(key=lambda item: item["mount"])
    return result[:MAX_FILESYSTEMS]


def to_int(text):
    return int(float(text))


def parse_lvm(vgs_out, lvs_out):
    """`vgs --noheadings --nosuffix --units b --separator ; -o vg_name,vg_size,vg_free,pv_count,lv_count` and
    `lvs ... -o vg_name,lv_name,lv_attr,lv_size,data_percent,metadata_percent` -> list of volume groups. `fill` lists what can run full
    on its own: thin pools and old style snapshots (a snapshot that is full is lost)."""
    groups = {}
    order = []
    for line in (vgs_out or "").splitlines():
        parts = [part.strip() for part in line.strip().split(";")]
        if len(parts) < 5 or not parts[0]:
            continue
        try:
            group = {"name": clean_name(parts[0]), "size": to_int(parts[1]), "free": to_int(parts[2]),
                     "pvs": to_int(parts[3]), "lvs": to_int(parts[4]), "fill": []}
        except ValueError:
            continue
        groups[parts[0]] = group
        order.append(group)
    for line in (lvs_out or "").splitlines():
        parts = [part.strip() for part in line.strip().split(";")]
        if len(parts) < 6 or parts[0] not in groups or parts[2][:1] not in ("t", "s", "S"):
            continue
        try:
            size = to_int(parts[3])
            data = float(parts[4]) if parts[4] else None
            meta = float(parts[5]) if parts[5] else None
        except ValueError:
            continue
        if data is None and meta is None:
            continue
        groups[parts[0]]["fill"].append({"name": clean_name(parts[1]), "kind": "thin-pool" if parts[2][0] == "t" else "snapshot",
                                         "size": size, "data_pct": data, "meta_pct": meta})
    return order[:MAX_VGS]


def lvm_collect(cfg, now=None, which=None):
    """Runs as root (its own timer). Writes lvm.json; the unprivileged `collect` reads it. Nothing is written on a host without LVM."""
    import shutil
    now = time.time() if now is None else now
    path = os.path.join(cfg.data_dir, LVM_FILE)
    if (which or shutil.which)("vgs", path=SAFE_PATH) is None:
        try:
            os.remove(path)                        # LVM was removed from this host
        except OSError:
            pass
        return None
    common = ["--readonly", "--noheadings", "--nosuffix", "--units", "b", "--separator", ";"]
    vgs = cfg.run(["vgs"] + common + ["-o", "vg_name,vg_size,vg_free,pv_count,lv_count"])
    lvs = cfg.run(["lvs"] + common + ["-o", "vg_name,lv_name,lv_attr,lv_size,data_percent,metadata_percent"]) if vgs is not None else None
    if vgs is None:
        data = {"t": int(now), "vgs": [], "error": "vgs failed (it needs root)"}
    else:
        data = {"t": int(now), "vgs": parse_lvm(vgs, lvs)}
        if lvs is None:
            data["error"] = "lvs failed: thin pools and snapshots are not shown"
    os.makedirs(cfg.data_dir, exist_ok=True)
    write_atomic(path, json.dumps(data, separators=(",", ":")) + "\n", 0o640)
    if os.geteuid() == 0:
        try:
            import grp
            os.chown(path, -1, grp.getgrnam("hostinfo").gr_gid)     # so that the collector (user hostinfo) can read it
        except (ImportError, KeyError, OSError):
            pass
    return data


def read_lvm(cfg, now):
    """(volume groups or None, problem or None) from the file that `lvm-collect` writes. No file: this host has no LVM (or the timer is
    not installed), which is not a problem."""
    try:
        with open(os.path.join(cfg.data_dir, LVM_FILE), "r") as handle:
            data = json.load(handle)
    except FileNotFoundError:
        return None, None
    except (OSError, ValueError):
        return None, "lvm: lvm.json cannot be read"
    if not isinstance(data, dict) or not isinstance(data.get("vgs"), list):
        return None, "lvm: lvm.json is not valid"
    age = now - data["t"] if isinstance(data.get("t"), (int, float)) else None
    if age is None or age > LVM_MAX_AGE:
        return None, "lvm: the data is %s old (is hostinfo-lvm.timer running?)" % ("more than %d min" % (LVM_MAX_AGE // 60) if age is None else "%d min" % (age // 60))
    problem = ("lvm: %s" % clean_name(str(data["error"]))) if data.get("error") else None
    return data["vgs"][:MAX_VGS], problem


# ----------------------------------------------------------------------------------------------------------------- collect

def load_state(cfg):
    try:
        with open(os.path.join(cfg.data_dir, "state.json"), "r") as handle:
            state = json.load(handle)
        return state if isinstance(state, dict) else {}
    except (OSError, ValueError):
        return {}


def write_atomic(path, text, mode=None):
    tmp = path + ".tmp"
    with open(tmp, "w") as handle:
        handle.write(text)
    if mode is not None:
        os.chmod(tmp, mode)
    os.replace(tmp, path)


def sample_file(cfg, t):
    return os.path.join(cfg.data_dir, "samples-" + time.strftime("%Y%m%d", time.gmtime(t)) + ".ndjson")


def prune(cfg, now):
    limit = time.strftime("%Y%m%d", time.gmtime(now - RETENTION_DAYS * 86400))
    try:
        entries = os.listdir(cfg.data_dir)
    except OSError:
        return
    for entry in entries:
        match = re.match(r"^samples-(\d{8})\.ndjson$", entry)
        if match and match.group(1) < limit:
            try:
                os.remove(os.path.join(cfg.data_dir, entry))
            except OSError:
                pass


def collect(cfg, now=None, write=True):
    """Takes one sample. Returns the status dictionary."""
    now = time.time() if now is None else now
    errors = []
    state = load_state(cfg) if write else {}
    previous_t = state.get("t")
    dt = (now - previous_t) if isinstance(previous_t, (int, float)) else None
    valid = dt is not None and 0 < dt <= cfg.gap_limit
    clk = float(cfg.clk_tck)
    try:
        uptime = float(read_text(os.path.join(cfg.proc, "uptime")).split()[0])
    except (OSError, ValueError, IndexError):
        uptime = None
    before_uptime = state.get("up")
    if valid and uptime is not None and isinstance(before_uptime, (int, float)) and uptime < before_uptime:
        valid = False                      # the host was restarted since the last sample: all counters began again

    # processor
    cpu_info = {"cores": None, "idle_pct": None, "iowait_pct": None, "load": None}
    busy_seconds = None
    stat_now = None
    try:
        stat_now, cores = read_cpu_stat(cfg)
        cpu_info["cores"] = cores
        cpu_info["load"] = read_loadavg(cfg)
        before = state.get("stat")
        if valid and isinstance(before, list) and len(before) == STAT_FIELDS:
            total = sum(stat_now) - sum(before)
            if total > 0:
                cpu_info["idle_pct"] = round(100.0 * (stat_now[3] - before[3]) / total, 1)
                cpu_info["iowait_pct"] = round(100.0 * (stat_now[4] - before[4]) / total, 1)
                busy_seconds = max(total - (stat_now[3] - before[3]) - (stat_now[4] - before[4]), 0) / clk
    except (OSError, ValueError, IndexError) as error:
        errors.append("cpu: %s" % error)

    # memory
    memory = None
    try:
        memory = read_meminfo(cfg)
        arc = read_zfs_arc(cfg)
        if arc is not None:
            memory["zfs_arc"] = arc
    except (OSError, ValueError) as error:
        errors.append("memory: %s" % error)

    # VMs: the CPU time used since the last sample, per VM
    vm_state = {}
    vm_list = []
    vm_seconds = {}
    old_vms = state.get("vms") if isinstance(state.get("vms"), dict) else {}
    for name, pid in list_vms(cfg).items():
        if not is_qemu(cfg, pid):
            continue
        try:
            ticks, start, rss = read_process(cfg, pid)
        except (OSError, ValueError, IndexError) as error:
            errors.append("vm %s: %s" % (name, error))
            continue
        vm_state[name] = {"pid": pid, "start": start, "ticks": ticks}
        used = None
        if valid:
            old = old_vms.get(name)
            if isinstance(old, dict) and old.get("pid") == pid and old.get("start") == start:
                used = max(ticks - old.get("ticks", 0), 0) / clk
            elif uptime is not None and isinstance(before_uptime, (int, float)) and start / clk > before_uptime:
                used = ticks / clk            # started (or restarted) since the last sample: everything it used is new
            # else: it was already running but not seen before: its first reading is only the starting point
            if used is not None:
                vm_seconds[name] = used
        vm_list.append({"name": name, "pid": pid, "rss": rss,
                        "cores": round(used / dt, 2) if (used is not None and dt) else None})
    vm_list.sort(key=lambda item: -(item["cores"] or 0))

    other_seconds = None
    if busy_seconds is not None and valid:
        other_seconds = max(busy_seconds - sum(vm_seconds.values()), 0.0)

    # storage: only asked every few minutes
    storage = None
    storage_problems = []
    stored = state.get("storage")
    if isinstance(stored, dict) and isinstance(stored.get("t"), (int, float)) and 0 <= now - stored["t"] < STORAGE_REFRESH:
        storage = stored
        storage_problems = [p for p in (state.get("storage_problems") or []) if isinstance(p, str)]
    else:
        try:
            storage = read_storage(cfg)
            storage["t"] = int(now)
            storage["fs"] = read_filesystems(cfg)
            groups, problem = read_lvm(cfg, now)
            if groups is not None:
                storage["lvm"] = groups
            if problem:
                storage_problems.append(problem)
        except Exception as error:         # a broken zfs must never stop the rest
            errors.append("storage: %s" % error)
            storage = stored if isinstance(stored, dict) else None
    errors.extend(storage_problems)

    status = {
        "v": FORMAT, "host": clean_name(os.uname().nodename), "t": int(now), "interval": cfg.interval,
        "cpu": cpu_info, "mem": memory, "storage": storage,
        "vms": {"count": len(vm_list), "list": vm_list},
        "other_cores": round(other_seconds / dt, 2) if (other_seconds is not None and dt) else None,
        "agent": {"version": VERSION, "python": "%d.%d.%d" % sys.version_info[:3]},
        "errors": errors,
    }
    if not write:
        return status

    os.umask(0o027)
    os.makedirs(cfg.data_dir, exist_ok=True)
    if valid and other_seconds is not None:
        line = {"t": int(now), "d": round(dt, 1), "c": {n: round(s, 2) for n, s in vm_seconds.items() if s is not None},
                "o": round(other_seconds, 2), "i": cpu_info["idle_pct"]}
        with open(sample_file(cfg, now), "a") as handle:
            handle.write(json.dumps(line, separators=(",", ":")) + "\n")
    write_atomic(os.path.join(cfg.data_dir, "status.json"), json.dumps(status, separators=(",", ":")) + "\n", 0o640)
    new_state = {"t": now, "up": uptime, "stat": stat_now, "vms": vm_state, "storage": storage, "storage_problems": storage_problems}
    write_atomic(os.path.join(cfg.data_dir, "state.json"), json.dumps(new_state, separators=(",", ":")), 0o600)
    prune(cfg, now)
    return status


# ----------------------------------------------------------------------------------------------------------------- serve

def read_status(cfg):
    with open(os.path.join(cfg.data_dir, "status.json"), "r") as handle:
        data = json.load(handle)
    if not isinstance(data, dict):
        raise ValueError("status.json is not an object")
    return data


def top(cfg, window_name, now=None):
    """The VMs by the CPU time they used in the last 1h/6h/24h/7d, with the average and the highest load."""
    now = time.time() if now is None else now
    seconds = WINDOWS[window_name]
    start = now - seconds
    totals = {}
    peaks = {}
    peak_times = {}
    covered = 0.0
    samples = 0
    other_total = 0.0
    day = int(start // 86400)
    while day <= int(now // 86400):
        path = os.path.join(cfg.data_dir, "samples-" + time.strftime("%Y%m%d", time.gmtime(day * 86400)) + ".ndjson")
        day += 1
        try:
            handle = open(path, "r")
        except OSError:
            continue
        with handle:
            for raw in handle:
                try:
                    item = json.loads(raw)
                    t, d, c = item["t"], float(item["d"]), item["c"]
                except (ValueError, KeyError, TypeError):
                    continue                  # a half written last line or damaged data: skip it
                if not (start < t <= now) or d <= 0 or not isinstance(c, dict):
                    continue
                samples += 1
                covered += d
                other_total += float(item.get("o") or 0)
                for name, used in c.items():
                    if not isinstance(used, (int, float)):
                        continue
                    totals[name] = totals.get(name, 0.0) + used
                    if used / d > peaks.get(name, -1):
                        peaks[name] = used / d
                        peak_times[name] = t
    try:
        cores = read_status(cfg).get("cpu", {}).get("cores") or os.cpu_count() or 1
    except (OSError, ValueError):
        cores = os.cpu_count() or 1
    rows = []
    for name, used in sorted(totals.items(), key=lambda pair: -pair[1]):
        average = used / covered if covered else 0.0
        rows.append({"name": name, "cpu_s": round(used, 1), "avg_cores": round(average, 2),
                     "avg_pct": round(100.0 * average / cores, 2), "peak_cores": round(peaks[name], 2), "peak_t": peak_times[name]})
    return {"v": FORMAT, "host": clean_name(os.uname().nodename), "window": window_name, "t": int(now), "cores": cores,
            "covered_s": round(covered, 1), "coverage": round(covered / seconds, 3), "samples": samples,
            "other_cpu_s": round(other_total, 1), "vms": rows}


def serve(cfg, command, out):
    """Answers one command. Only the exact words below are accepted: nothing of the command is ever given to a shell or used as a path."""
    words = command.split()
    try:
        if words == ["status"]:
            result = read_status(cfg)
        elif words == ["version"]:
            result = {"v": FORMAT, "version": VERSION}
        elif len(words) == 2 and words[0] == "top" and words[1] in WINDOWS:
            result = top(cfg, words[1])
        else:
            out.write(json.dumps({"error": "command not allowed"}) + "\n")
            return 2
    except (OSError, ValueError) as error:
        out.write(json.dumps({"error": "no data: %s" % error.__class__.__name__}) + "\n")
        return 1
    out.write(json.dumps(result, separators=(",", ":")) + "\n")
    return 0


def main(argv):
    if len(argv) < 2 or argv[1] not in ("collect", "lvm-collect", "serve", "version"):
        sys.stderr.write("usage: hostinfo collect [--dry-run] | lvm-collect | serve [command] | version\n")
        return 2
    cfg = Config()
    if argv[1] == "lvm-collect":
        lvm_collect(cfg)
        return 0
    if argv[1] == "version":
        print(VERSION)
        return 0
    if argv[1] == "collect":
        dry = "--dry-run" in argv[2:]
        if not dry:
            # two runs must not overlap (a slow zpool on a busy host)
            import fcntl
            os.makedirs(cfg.data_dir, exist_ok=True)
            lock = open(os.path.join(cfg.data_dir, "lock"), "w")
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError:
                return 0
        status = collect(cfg, write=not dry)
        if dry:
            print(json.dumps(status, indent=2))
        return 0
    # serve: from ssh the command is in SSH_ORIGINAL_COMMAND; at a terminal it can be given as arguments (for testing)
    command = os.environ.get("SSH_ORIGINAL_COMMAND")
    if command is None:
        command = " ".join(argv[2:])
    return serve(cfg, command, sys.stdout)


if __name__ == "__main__":
    sys.exit(main(sys.argv))
