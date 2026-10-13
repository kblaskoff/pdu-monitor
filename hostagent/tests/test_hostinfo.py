import importlib.util
import io
import json
import os
import shutil
import stat as statmod
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("hostinfo", os.path.join(HERE, "..", "hostinfo.py"))
hostinfo = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hostinfo)

CLK = 100
T0 = 1_760_000_000          # a fixed "now" so that the tests do not depend on the clock


class FakeHost(object):
    """A fake /proc, a fake libvirt directory and a data directory."""

    def __init__(self, cores=4, run=None):
        self.root = tempfile.mkdtemp()
        self.proc = os.path.join(self.root, "proc")
        self.libvirt = os.path.join(self.root, "libvirt")
        self.data = os.path.join(self.root, "data")
        os.makedirs(self.proc)
        os.makedirs(self.libvirt)
        self.cores = cores
        self.cfg = hostinfo.Config(data_dir=self.data, proc=self.proc, libvirt=self.libvirt, interval=60, clk_tck=CLK,
                                   run=run or (lambda argv: None))
        self.stat(user=0, system=0, idle=0, iowait=0)
        self.write("loadavg", "96.10 95.50 94.00 3/900 12345\n")
        self.write("meminfo", "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 400 kB\nSwapTotal: 50 kB\nSwapFree: 40 kB\n")
        self.write("uptime", "1000.00 5000.00\n")

    def cleanup(self):
        shutil.rmtree(self.root, ignore_errors=True)

    def write(self, relative, text):
        path = os.path.join(self.proc, relative)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as handle:
            handle.write(text)

    def stat(self, user, system, idle, iowait):
        lines = ["cpu  %d 0 %d %d %d 0 0 0 0 0" % (user, system, idle, iowait)]
        for i in range(self.cores):
            lines.append("cpu%d 1 0 1 1 1 0 0 0 0 0" % i)
        lines.append("intr 1 2 3")
        self.write("stat", "\n".join(lines) + "\n")

    def uptime(self, seconds):
        self.write("uptime", "%.2f 1.00\n" % seconds)

    def vm(self, name, pid, ticks, start=10_000, comm="qemu-kvm"):
        with open(os.path.join(self.libvirt, name + ".pid"), "w") as handle:
            handle.write("%d\n" % pid)
        # a name with spaces and a bracket on purpose: "(qemu kvm (x))"
        utime, stime = ticks // 2, ticks - ticks // 2
        fields = ["S", "1", "1", "1", "0", "-1", "0", "0", "0", "0", "0", str(utime), str(stime), "0", "0", "20", "0", "30", "0", str(start)]
        self.write("%d/stat" % pid, "%d (qemu kvm (x)) %s\n" % (pid, " ".join(fields)))
        self.write("%d/statm" % pid, "1000 250 0 0 0 0 0\n")
        self.write("%d/comm" % pid, comm + "\n")

    def remove_vm(self, name, pid):
        os.remove(os.path.join(self.libvirt, name + ".pid"))
        shutil.rmtree(os.path.join(self.proc, str(pid)), ignore_errors=True)


class CollectTests(unittest.TestCase):
    def setUp(self):
        self.host = FakeHost()
        self.addCleanup(self.host.cleanup)
        self.cfg = self.host.cfg

    def samples(self, t=T0):
        path = hostinfo.sample_file(self.cfg, t)
        with open(path) as handle:
            return [json.loads(line) for line in handle]

    def test_first_run_has_a_status_but_no_rates_and_no_sample(self):
        self.host.vm("web1", 111, 5000)
        status = hostinfo.collect(self.cfg, now=T0)
        self.assertEqual(status["cpu"]["cores"], 4)
        self.assertEqual(status["cpu"]["load"], [96.1, 95.5, 94.0])
        self.assertIsNone(status["cpu"]["idle_pct"])
        self.assertEqual(status["vms"]["count"], 1)
        self.assertIsNone(status["vms"]["list"][0]["cores"])
        self.assertFalse(os.path.exists(hostinfo.sample_file(self.cfg, T0)))
        self.assertEqual(status["mem"], {"total": 1024000, "available": 409600, "used": 614400, "swap_used": 10240})

    def test_cpu_of_each_vm_is_the_difference_of_the_counters(self):
        self.host.vm("web1", 111, 5000)
        self.host.vm("db1", 222, 1000)
        self.host.stat(user=1000, system=0, idle=9000, iowait=0)
        hostinfo.collect(self.cfg, now=T0)
        # 60 s later: web1 used 600 ticks = 6 s, db1 used 3000 ticks = 30 s; the host was busy 40 s of 4 cores * 60 s = 24000 ticks
        self.host.vm("web1", 111, 5600)
        self.host.vm("db1", 222, 4000)
        self.host.uptime(1060)
        self.host.stat(user=1000 + 4000, system=0, idle=9000 + 20000, iowait=0)
        status = hostinfo.collect(self.cfg, now=T0 + 60)
        sample = self.samples()[0]
        self.assertEqual(sample["d"], 60.0)
        self.assertEqual(sample["c"], {"web1": 6.0, "db1": 30.0})
        self.assertAlmostEqual(sample["o"], 4.0, places=2)           # busy 40 s - 36 s of VMs = 4 s of the host itself
        self.assertAlmostEqual(status["cpu"]["idle_pct"], round(100.0 * 20000 / 24000, 1))
        by_name = {v["name"]: v for v in status["vms"]["list"]}
        self.assertEqual(by_name["db1"]["cores"], 0.5)
        self.assertEqual(by_name["web1"]["cores"], 0.1)
        self.assertEqual(status["vms"]["list"][0]["name"], "db1")     # the busiest first
        self.assertEqual(by_name["web1"]["rss"], 250 * os.sysconf("SC_PAGE_SIZE"))

    def advance(self, seconds_after, ticks):
        """Second reading with the given counters of the VMs, hosts counters advancing like an idle machine."""
        for name, pid, count, start in ticks:
            self.host.vm(name, pid, count, start)
        self.host.uptime(1000 + seconds_after)
        self.host.stat(user=100 * seconds_after, system=0, idle=1000 * seconds_after, iowait=0)

    def test_restarted_vm_counts_what_it_used_since_it_started(self):
        self.host.vm("web1", 111, 90_000, start=10_000)
        hostinfo.collect(self.cfg, now=T0)
        # web1 was restarted 20 s ago (start = 1040 s of uptime * 100 ticks), new pid, 800 ticks used so far
        self.advance(60, [("web1", 333, 800, 104_000)])
        hostinfo.collect(self.cfg, now=T0 + 60)
        self.assertEqual(self.samples()[0]["c"], {"web1": 8.0})

    def test_vm_that_was_already_running_but_not_seen_is_only_a_starting_point(self):
        hostinfo.collect(self.cfg, now=T0)
        self.advance(60, [("old", 444, 999_999, 100)])               # started long before the previous sample
        hostinfo.collect(self.cfg, now=T0 + 60)
        self.assertEqual(self.samples()[0]["c"], {})
        self.advance(120, [("old", 444, 1_000_599, 100)])
        hostinfo.collect(self.cfg, now=T0 + 120)
        self.assertEqual(self.samples()[1]["c"], {"old": 6.0})

    def test_new_vm_started_after_the_last_sample_counts_from_its_start(self):
        hostinfo.collect(self.cfg, now=T0)
        self.advance(60, [("fresh", 555, 1200, 105_000)])             # started at uptime 1050 s, after the sample (1000 s)
        hostinfo.collect(self.cfg, now=T0 + 60)
        self.assertEqual(self.samples()[0]["c"], {"fresh": 12.0})

    def test_a_long_gap_is_not_one_interval(self):
        self.host.vm("web1", 111, 100)
        hostinfo.collect(self.cfg, now=T0)
        self.advance(3600, [("web1", 111, 100_000, 10_000)])
        hostinfo.collect(self.cfg, now=T0 + 3600)
        self.assertFalse(os.path.exists(hostinfo.sample_file(self.cfg, T0 + 3600)))
        # the next one is good again
        self.advance(3660, [("web1", 111, 100_600, 10_000)])
        hostinfo.collect(self.cfg, now=T0 + 3660)
        self.assertEqual(self.samples(T0 + 3660)[-1]["c"], {"web1": 6.0})

    def test_a_reboot_between_two_samples_is_not_counted(self):
        self.host.uptime(5000)
        self.host.vm("web1", 111, 900_000)
        hostinfo.collect(self.cfg, now=T0)
        self.host.uptime(30)                                          # the machine started again
        self.host.stat(user=5, system=5, idle=50, iowait=0)
        self.host.vm("web1", 999, 100, start=100)
        hostinfo.collect(self.cfg, now=T0 + 60)
        self.assertFalse(os.path.exists(hostinfo.sample_file(self.cfg, T0 + 60)))

    def test_pid_file_of_something_else_and_unreadable_vm_do_not_stop_the_sample(self):
        self.host.vm("good", 111, 100)
        self.host.vm("reused", 222, 100, comm="bash")                 # the pid was reused by another program
        with open(os.path.join(self.host.libvirt, "broken.pid"), "w") as handle:
            handle.write("333\n")                                     # no /proc/333
        with open(os.path.join(self.host.libvirt, "garbage.pid"), "w") as handle:
            handle.write("not a number\n")
        status = hostinfo.collect(self.cfg, now=T0)
        self.assertEqual([v["name"] for v in status["vms"]["list"]], ["good"])
        self.assertTrue(any("broken" in e for e in status["errors"]))

    def test_vm_names_are_cleaned(self):
        self.host.vm("we\x1bird\nname", 111, 100)
        status = hostinfo.collect(self.cfg, now=T0)
        self.assertEqual(status["vms"]["list"][0]["name"], "weirdname")
        self.assertEqual(hostinfo.clean_name("x" * 300), "x" * 100)

    def test_files_are_atomic_and_not_readable_by_everybody(self):
        hostinfo.collect(self.cfg, now=T0)
        status_path = os.path.join(self.host.data, "status.json")
        with open(status_path) as handle:
            self.assertEqual(json.load(handle)["v"], 1)
        self.assertFalse(os.path.exists(status_path + ".tmp"))
        self.assertEqual(statmod.S_IMODE(os.stat(status_path).st_mode), 0o640)
        self.assertEqual(statmod.S_IMODE(os.stat(os.path.join(self.host.data, "state.json")).st_mode), 0o600)

    def test_old_sample_files_are_removed(self):
        os.makedirs(self.host.data)
        old = os.path.join(self.host.data, "samples-%s.ndjson" % time.strftime("%Y%m%d", time.gmtime(T0 - 9 * 86400)))
        recent = os.path.join(self.host.data, "samples-%s.ndjson" % time.strftime("%Y%m%d", time.gmtime(T0 - 2 * 86400)))
        other = os.path.join(self.host.data, "notes.txt")
        for path in (old, recent, other):
            open(path, "w").close()
        hostinfo.collect(self.cfg, now=T0)
        self.assertFalse(os.path.exists(old))
        self.assertTrue(os.path.exists(recent))
        self.assertTrue(os.path.exists(other))

    def test_dry_run_writes_nothing(self):
        status = hostinfo.collect(self.cfg, now=T0, write=False)
        self.assertEqual(status["v"], 1)
        self.assertFalse(os.path.exists(self.host.data))


class StorageTests(unittest.TestCase):
    ZPOOL = "pool\t91534343012352\t59918395113472\t31615947898880\t65\tONLINE\n"
    ZFS = "pool\t53246036254320\t27960521023888\n"

    def test_parse_gives_raw_and_usable_space(self):
        pools = hostinfo.parse_zfs(self.ZPOOL, self.ZFS)
        self.assertEqual(len(pools), 1)
        pool = pools[0]
        self.assertEqual(pool["health"], "ONLINE")
        self.assertEqual(pool["size"], 91534343012352)
        self.assertEqual(pool["usable_total"], 53246036254320 + 27960521023888)
        self.assertEqual(pool["cap_pct"], 65)

    def test_parse_survives_garbage(self):
        self.assertEqual(hostinfo.parse_zfs("garbage\nx\ty\n", "also\tbad\n"), [])
        self.assertEqual(hostinfo.parse_zfs(None, None), [])

    def test_storage_is_asked_every_five_minutes_only_and_degraded_is_reported(self):
        calls = []

        def fake(argv):
            calls.append(argv[0])
            return self.ZPOOL.replace("ONLINE", "DEGRADED") if argv[0] == "zpool" else self.ZFS

        host = FakeHost(run=fake)
        self.addCleanup(host.cleanup)
        first = hostinfo.collect(host.cfg, now=T0)
        self.assertEqual(first["storage"]["pools"][0]["health"], "DEGRADED")
        self.assertEqual(calls, ["zpool", "zfs"])
        hostinfo.collect(host.cfg, now=T0 + 60)
        hostinfo.collect(host.cfg, now=T0 + 120)
        self.assertEqual(calls, ["zpool", "zfs"])                      # cached
        host.uptime(1400)
        hostinfo.collect(host.cfg, now=T0 + 310)
        self.assertEqual(calls, ["zpool", "zfs", "zpool", "zfs"])

    def test_host_without_zfs(self):
        host = FakeHost(run=lambda argv: None)
        self.addCleanup(host.cleanup)
        self.assertEqual(hostinfo.collect(host.cfg, now=T0)["storage"]["pools"], [])

    def test_arc_is_reported_as_part_of_memory(self):
        host = FakeHost()
        self.addCleanup(host.cleanup)
        host.write("spl/kstat/zfs/arcstats", "13 1 0x01 123 6396 1 1\nname type data\nhits 4 5\nsize 4 777\n")
        self.assertEqual(hostinfo.collect(host.cfg, now=T0)["mem"]["zfs_arc"], 777)


class FakeStatvfs(object):
    def __init__(self, blocks, free, avail, frsize=4096):
        self.f_blocks, self.f_bfree, self.f_bavail, self.f_frsize = blocks, free, avail, frsize


VGS = "  am;22888000000000;4806000000000;1;133\n  vg2;1000;1000;2;0\n"
LVS = ("  am;kvm1_img;-wi-ao----;21474836480;;\n"
       "  am;thin;twi-aotz--;1000000000000;42.50;7.25\n"
       "  am;snap1;swi-a-s---;5000000000;91.00;\n"
       "  am;thinvol;Vwi-a-tz--;1000;3.00;\n"
       "  other;x;twi-a-tz--;1;1.0;1.0\n")


class LvmTests(unittest.TestCase):
    def setUp(self):
        self.host = FakeHost()
        self.addCleanup(self.host.cleanup)
        self.cfg = self.host.cfg

    def test_parse_gives_groups_and_what_can_fill_up(self):
        groups = hostinfo.parse_lvm(VGS, LVS)
        self.assertEqual([g["name"] for g in groups], ["am", "vg2"])
        am = groups[0]
        self.assertEqual((am["size"], am["free"], am["pvs"], am["lvs"]), (22888000000000, 4806000000000, 1, 133))
        self.assertEqual([(f["name"], f["kind"], f["data_pct"], f["meta_pct"]) for f in am["fill"]],
                         [("thin", "thin-pool", 42.5, 7.25), ("snap1", "snapshot", 91.0, None)])      # plain volumes and thin volumes are not listed
        self.assertEqual(groups[1]["fill"], [])

    def test_parse_survives_garbage(self):
        self.assertEqual(hostinfo.parse_lvm("garbage\n;;;;\nx;y;z;1;2\n", "a;b\n"), [])
        self.assertEqual(hostinfo.parse_lvm(None, None), [])
        self.assertEqual(len(hostinfo.parse_lvm("  am;1;1;1;1\n", "am;t;twi-a-tz--;1;oops;1\n")[0]["fill"]), 0)

    def test_lvm_collect_asks_with_fixed_arguments_and_writes_the_file(self):
        calls = []

        def fake(argv):
            calls.append(argv)
            return VGS if argv[0] == "vgs" else LVS

        host = FakeHost(run=fake)
        self.addCleanup(host.cleanup)
        data = hostinfo.lvm_collect(host.cfg, now=T0, which=lambda name, path=None: "/sbin/" + name)
        self.assertEqual([c[0] for c in calls], ["vgs", "lvs"])
        for argv in calls:
            self.assertIn("--readonly", argv)             # no locks, nothing is changed
        self.assertEqual(data["t"], T0)
        with open(os.path.join(host.data, "lvm.json")) as handle:
            self.assertEqual(json.load(handle)["vgs"][0]["name"], "am")
        self.assertEqual(statmod.S_IMODE(os.stat(os.path.join(host.data, "lvm.json")).st_mode), 0o640)

    def test_no_lvm_on_the_host_leaves_no_file_and_removes_an_old_one(self):
        os.makedirs(self.host.data)
        path = os.path.join(self.host.data, "lvm.json")
        with open(path, "w") as handle:
            handle.write("{}")
        self.assertIsNone(hostinfo.lvm_collect(self.cfg, now=T0, which=lambda name, path=None: None))
        self.assertFalse(os.path.exists(path))

    def test_failing_vgs_is_a_visible_problem(self):
        host = FakeHost(run=lambda argv: None)
        self.addCleanup(host.cleanup)
        data = hostinfo.lvm_collect(host.cfg, now=T0, which=lambda name, path=None: "/sbin/vgs")
        self.assertEqual(data["vgs"], [])
        self.assertIn("root", data["error"])
        groups, problem = hostinfo.read_lvm(host.cfg, T0 + 10)
        self.assertEqual(groups, [])
        self.assertIn("vgs failed", problem)

    def test_collect_picks_up_the_file_and_reports_a_stale_one(self):
        host = FakeHost()
        self.addCleanup(host.cleanup)
        os.makedirs(host.data)
        with open(os.path.join(host.data, "lvm.json"), "w") as handle:
            json.dump({"t": T0 - 30, "vgs": hostinfo.parse_lvm(VGS, LVS)}, handle)
        status = hostinfo.collect(host.cfg, now=T0)
        self.assertEqual(status["storage"]["lvm"][0]["name"], "am")
        self.assertEqual(status["errors"], [])
        # the file is not renewed (the timer stopped): after 20 minutes it is not shown any more and the reason is said
        host.uptime(5000)
        later = hostinfo.collect(host.cfg, now=T0 + 1300)
        self.assertNotIn("lvm", later["storage"])
        self.assertTrue(any("lvm: the data is" in e for e in later["errors"]), later["errors"])
        # and the problem stays visible between the five-minute readings of the storage
        host.uptime(5060)
        again = hostinfo.collect(host.cfg, now=T0 + 1360)
        self.assertTrue(any("lvm: the data is" in e for e in again["errors"]), again["errors"])

    def test_without_the_file_nothing_is_said(self):
        status = hostinfo.collect(self.cfg, now=T0)
        self.assertNotIn("lvm", status["storage"])
        self.assertEqual(status["errors"], [])

    def test_damaged_file_does_not_stop_the_sample(self):
        os.makedirs(self.host.data)
        for text in ("{not json", "[]", '{"t": 1, "vgs": 5}'):
            with open(os.path.join(self.host.data, "lvm.json"), "w") as handle:
                handle.write(text)
            groups, problem = hostinfo.read_lvm(self.cfg, T0)
            self.assertIsNone(groups)
            self.assertTrue(problem.startswith("lvm:"), text)


class FilesystemTests(unittest.TestCase):
    MOUNTS = ("rootfs / rootfs rw 0 0\n"
              "proc /proc proc rw 0 0\n"
              "/dev/md126 / ext4 rw,relatime 0 0\n"
              "/dev/md127 /boot ext4 rw 0 0\n"
              "/dev/md125 /boot/efi vfat rw 0 0\n"
              "/dev/mapper/am-solusvm /home/solusvm ext4 rw 0 0\n"
              "/dev/mapper/am-solusvm /var/lib/bind ext4 rw 0 0\n"                  # the same device again: a bind mount
              "/dev/sdb1 /mnt/with\\040space xfs rw 0 0\n"
              "pool /pool zfs rw 0 0\n"
              "/dev/sdc1 /gone ext4 rw 0 0\n")

    def test_lists_local_filesystems_once_and_skips_the_rest(self):
        sizes = {"/": (100, 40, 35), "/boot": (10, 8, 8), "/home/solusvm": (50, 20, 20), "/var/lib/bind": (50, 20, 20), "/mnt/with space": (10, 0, 0)}

        def statvfs(path):
            if path not in sizes:
                raise OSError("no such mount")
            b, f, a = sizes[path]
            return FakeStatvfs(b, f, a, frsize=1000)

        host = FakeHost()
        self.addCleanup(host.cleanup)
        host.cfg.statvfs = statvfs
        host.write("mounts", self.MOUNTS)
        fs = hostinfo.read_filesystems(host.cfg)
        self.assertEqual([f["mount"] for f in fs], ["/", "/boot", "/home/solusvm", "/mnt/with space"])      # no vfat, zfs, proc, bind mount or unreadable
        root = fs[0]
        self.assertEqual((root["dev"], root["type"], root["size"], root["used"], root["avail"]), ("/dev/md126", "ext4", 100000, 60000, 35000))

    def test_collect_includes_them_and_survives_a_missing_mounts_file(self):
        host = FakeHost()
        self.addCleanup(host.cleanup)
        self.assertEqual(hostinfo.collect(host.cfg, now=T0)["storage"]["fs"], [])
        host2 = FakeHost()
        self.addCleanup(host2.cleanup)
        host2.cfg.statvfs = lambda path: FakeStatvfs(1000, 500, 400)
        host2.write("mounts", "/dev/a / ext4 rw 0 0\n")
        self.assertEqual(hostinfo.collect(host2.cfg, now=T0)["storage"]["fs"][0]["mount"], "/")

    def test_names_are_cleaned_and_the_list_is_limited(self):
        host = FakeHost()
        self.addCleanup(host.cleanup)
        host.cfg.statvfs = lambda path: FakeStatvfs(10, 5, 5)
        host.write("mounts", "".join("/dev/d%d /m%02d ext4 rw 0 0\n" % (i, i) for i in range(60)) + "/dev/x /ctl\\033x ext4 rw 0 0\n")
        fs = hostinfo.read_filesystems(host.cfg)
        self.assertEqual(len(fs), hostinfo.MAX_FILESYSTEMS)
        self.assertTrue(all(ch >= " " for item in fs for ch in item["mount"]))


class ServeTests(unittest.TestCase):
    def setUp(self):
        self.host = FakeHost()
        self.addCleanup(self.host.cleanup)
        self.cfg = self.host.cfg

    def run_command(self, command):
        out = io.StringIO()
        code = hostinfo.serve(self.cfg, command, out)
        return code, json.loads(out.getvalue())

    def add_samples(self, rows):
        os.makedirs(self.host.data, exist_ok=True)
        by_day = {}
        for row in rows:
            by_day.setdefault(hostinfo.sample_file(self.cfg, row["t"]), []).append(row)
        for path, items in by_day.items():
            with open(path, "a") as handle:
                for item in items:
                    handle.write(json.dumps(item) + "\n")

    def test_only_the_exact_commands_are_answered(self):
        hostinfo.collect(self.cfg, now=T0)
        self.assertEqual(self.run_command("status")[0], 0)
        self.assertEqual(self.run_command("version")[1]["version"], hostinfo.VERSION)
        for bad in ["", "STATUS", "status; id", "status && id", "top", "top 2h", "top 6h extra", "top ../../etc/passwd", "top 6h; rm -rf /",
                    "$(id)", "`id`", "cat /etc/shadow", "top 7d\nid", "status|cat", "top  ", "-h", "collect"]:
            code, answer = self.run_command(bad)
            self.assertEqual(code, 2, bad)
            self.assertEqual(answer, {"error": "command not allowed"}, bad)

    def test_no_data_yet(self):
        code, answer = self.run_command("status")
        self.assertEqual(code, 1)
        self.assertIn("error", answer)

    def test_top_sums_the_cpu_time_and_orders_the_vms(self):
        now = time.time()
        rows = []
        for i in range(120):                       # 2 hours of one minute samples
            t = int(now - 60 * i)
            rows.append({"t": t, "d": 60.0, "c": {"hot": 30.0, "warm": 6.0, "cold": 0.6}, "o": 2.0})
        rows.append({"t": int(now - 3000), "d": 60.0, "c": {"spike": 150.0}, "o": 0.0})     # one short peak of 2.5 cores
        self.add_samples(rows)
        hostinfo.collect(self.cfg, now=now)
        code, answer = self.run_command("top 1h")
        self.assertEqual(code, 0)
        names = [v["name"] for v in answer["vms"]]
        self.assertEqual(names, ["hot", "warm", "spike", "cold"])       # 1800 s, 360 s, 150 s, 36 s
        by_name = {v["name"]: v for v in answer["vms"]}
        # in the last hour: 60 samples of "hot" (30 s each) = 1800 s; spike 150 s once
        self.assertAlmostEqual(by_name["hot"]["cpu_s"], 30.0 * 60, delta=60)
        self.assertEqual(answer["vms"][0]["name"], "hot")
        self.assertEqual(by_name["spike"]["peak_cores"], 2.5)
        self.assertAlmostEqual(by_name["hot"]["avg_cores"], 0.5, places=1)
        self.assertEqual(answer["cores"], 4)
        self.assertAlmostEqual(by_name["hot"]["avg_pct"], 12.5, places=0)
        _, six = self.run_command("top 6h")
        self.assertGreater({v["name"]: v for v in six["vms"]}["hot"]["cpu_s"], by_name["hot"]["cpu_s"])      # 2 h > 1 h
        self.assertLess(six["coverage"], 0.4)                          # only 2 of 6 hours have samples: the screen can say so
        self.assertAlmostEqual(answer["coverage"], 1.0, places=1)

    def test_top_over_seven_days_reads_several_files(self):
        now = time.time()
        rows = [{"t": int(now - 86400 * d - 100), "d": 60.0, "c": {"daily": 60.0}, "o": 0.0} for d in range(7)]
        rows.append({"t": int(now - 8 * 86400), "d": 60.0, "c": {"too old": 60.0}, "o": 0.0})
        self.add_samples(rows)
        hostinfo.collect(self.cfg, now=now)
        _, answer = self.run_command("top 7d")
        self.assertEqual([v["name"] for v in answer["vms"]], ["daily"])
        self.assertEqual(answer["samples"], 7)
        self.assertEqual(answer["vms"][0]["cpu_s"], 420.0)

    def test_damaged_lines_are_skipped(self):
        now = int(time.time())
        os.makedirs(self.host.data, exist_ok=True)
        with open(hostinfo.sample_file(self.cfg, now), "w") as handle:
            handle.write('{"t": %d, "d": 60, "c": {"ok": 6}, "o": 0}\n' % (now - 10))
            handle.write("this is not json\n")
            handle.write('{"t": "x"}\n')
            handle.write('{"t": %d, "d": 60, "c": {"half' % (now - 5))               # the line that was being written
        hostinfo.collect(self.cfg, now=now)
        _, answer = self.run_command("top 1h")
        self.assertEqual([v["name"] for v in answer["vms"]], ["ok"])

    def test_empty_history(self):
        hostinfo.collect(self.cfg, now=time.time())
        _, answer = self.run_command("top 24h")
        self.assertEqual(answer["vms"], [])
        self.assertEqual(answer["samples"], 0)


class RealProcTests(unittest.TestCase):
    """Runs against the real /proc of the machine that runs the tests (Linux only)."""

    @unittest.skipUnless(os.path.exists("/proc/stat"), "needs Linux")
    def test_two_collections_on_this_machine(self):
        root = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, root, True)
        cfg = hostinfo.Config(data_dir=os.path.join(root, "data"), libvirt=os.path.join(root, "none"))
        first = hostinfo.collect(cfg, now=time.time() - 30)
        time.sleep(0.2)                    # the counters of /proc/stat tick every 10 ms
        second = hostinfo.collect(cfg, now=time.time())
        self.assertGreaterEqual(first["cpu"]["cores"], 1)
        self.assertEqual(second["errors"], [])
        self.assertIsNotNone(second["cpu"]["idle_pct"])
        self.assertGreater(second["mem"]["total"], 0)
        self.assertEqual(second["vms"]["count"], 0)


if __name__ == "__main__":
    unittest.main()
