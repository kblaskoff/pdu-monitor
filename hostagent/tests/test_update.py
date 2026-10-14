"""hostinfo-update (update.sh) against a fake curl and a fake archive: it must change nothing unless a new, complete version
is there, and keep the installed one whenever a step fails."""
import io
import os
import shutil
import subprocess
import tarfile
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
UPDATER = os.path.join(HERE, "..", "update.sh")
OLD = "1" * 40
NEW = "a" * 40

FAKE_CURL = r"""#!/bin/bash
echo "$*" >> "$FAKE_LOG"
url="${@: -1}"; out=""
while [[ $# -gt 0 ]]; do [[ "$1" == "-o" ]] && { out="$2"; shift; }; shift; done
case "$url" in
  *api.github.com*) [[ -f "$FAKE_API" ]] && cat "$FAKE_API" || exit 22 ;;
  *github.com/*/archive/*) [[ -f "$FAKE_TGZ" ]] && cp "$FAKE_TGZ" "$out" || exit 22 ;;
  *) exit 22 ;;
esac
"""

FAKE_INSTALL = r"""#!/bin/bash
echo "$@" > "$FAKE_INSTALLED"
[[ -z "${FAKE_INSTALL_FAIL:-}" ]] || { echo "useradd: broken"; exit 1; }
echo "installed from $(basename "$(dirname "$PWD")")"
"""


@unittest.skipUnless(all(shutil.which(c) for c in ("bash", "flock", "tar")), "needs bash, flock and tar")
class UpdaterTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        bin_dir = os.path.join(self.dir, "bin")
        os.mkdir(bin_dir)
        self.script(os.path.join(bin_dir, "curl"), FAKE_CURL)
        self.program = self.script(os.path.join(self.dir, "hostinfo"), "#!/bin/sh\necho 0.2.1\n")
        self.state = os.path.join(self.dir, "installed-commit")
        self.log = os.path.join(self.dir, "curl.log")
        self.installed = os.path.join(self.dir, "installed")
        self.env = dict(os.environ, PATH=bin_dir + os.pathsep + os.environ["PATH"], FAKE_LOG=self.log,
                        FAKE_API=os.path.join(self.dir, "api.json"), FAKE_TGZ=os.path.join(self.dir, "archive.tar.gz"),
                        FAKE_INSTALLED=self.installed, HOSTINFO_UPDATE_STATE=self.state,
                        HOSTINFO_UPDATE_LOCK=os.path.join(self.dir, "lock"), HOSTINFO_PROGRAM=self.program)

    def script(self, path, text):
        with open(path, "w") as handle:
            handle.write(text)
        os.chmod(path, 0o755)
        return path

    def github_says(self, text):
        with open(self.env["FAKE_API"], "w") as handle:
            handle.write(text)

    def latest_is(self, sha):
        self.github_says('[{"sha": "%s", "commit": {"message": "Host agent"}}]' % sha)

    def archive(self, sha, files=None):
        files = files if files is not None else {"hostagent/install.sh": FAKE_INSTALL, "hostagent/hostinfo.py": "VERSION = '0.2.1'\n"}
        with tarfile.open(self.env["FAKE_TGZ"], "w:gz") as tar:
            for name, text in files.items():
                data = text.encode()
                info = tarfile.TarInfo("pdu-monitor-%s/%s" % (sha, name))
                info.size, info.mode = len(data), 0o755
                tar.addfile(info, io.BytesIO(data))

    def installed_now(self, sha):
        with open(self.state, "w") as handle:
            handle.write(sha + "\n")

    def run_updater(self, *args, **env):
        result = subprocess.run(["bash", UPDATER] + list(args), env=dict(self.env, **env),
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=60)
        return result.returncode, result.stdout + result.stderr

    def state_is(self):
        with open(self.state) as handle:
            return handle.read().strip()

    def downloads(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log) as handle:
            return [line for line in handle if "/archive/" in line]

    def test_nothing_is_downloaded_when_the_installed_commit_is_the_latest(self):
        self.installed_now(NEW)
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater()
        self.assertEqual(code, 0, out)
        self.assertIn("Up to date: 0.2.1", out)
        self.assertEqual(self.downloads(), [])
        self.assertFalse(os.path.exists(self.installed))
        self.assertEqual(self.run_updater("--quiet"), (0, ""))

    def test_a_new_commit_is_installed_from_its_own_archive_with_update(self):
        self.installed_now(OLD)
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater("--quiet")
        self.assertEqual(code, 0, out)
        self.assertIn("Updated hostinfo 0.2.1 -> 0.2.1 (commit aaaaaaa)", out)
        self.assertEqual(self.state_is(), NEW)
        with open(self.installed) as handle:
            self.assertEqual(handle.read().strip(), "--update")       # never --key: the key line stays
        self.assertEqual(len(self.downloads()), 1)
        self.assertIn("/archive/%s.tar.gz" % NEW, self.downloads()[0])  # the commit itself, not main that may move meanwhile

    def test_without_a_state_file_the_first_run_installs(self):
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater()
        self.assertEqual(code, 0, out)
        self.assertEqual(self.state_is(), NEW)

    def test_check_only_tells(self):
        self.installed_now(OLD)
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater("--check")
        self.assertEqual(code, 10, out)
        self.assertIn("commit aaaaaaa (installed: 1111111)", out)
        self.assertEqual(self.downloads(), [])
        self.assertEqual(self.state_is(), OLD)

    def test_force_installs_the_same_commit_again(self):
        self.installed_now(NEW)
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater("--force")
        self.assertEqual(code, 0, out)
        self.assertTrue(os.path.exists(self.installed))

    def test_a_strange_answer_from_github_changes_nothing(self):
        self.archive(NEW)
        for answer in ("", "[]", "not json", '[{"sha": "main"}]', '[{"sha": "%s; rm -rf /"}]' % NEW, '[{"sha": "%s"}]' % NEW.upper(),
                       '{"message": "API rate limit exceeded"}'):
            self.installed_now(OLD)
            self.github_says(answer)
            code, out = self.run_updater()
            self.assertEqual(code, 1, (answer, out))
            self.assertEqual(self.state_is(), OLD, answer)
            self.assertFalse(os.path.exists(self.installed), answer)
            self.assertEqual(self.downloads(), [], answer)             # refused before anything is fetched
        os.remove(self.env["FAKE_API"])                                # GitHub cannot be reached at all
        code, out = self.run_updater()
        self.assertEqual(code, 1, out)
        self.assertIn("GitHub did not answer", out)

    def test_a_broken_download_or_archive_changes_nothing(self):
        cases = [
            ("no archive", None),
            ("no hostagent folder", {"README.md": "x"}),
            ("an archive of another commit", "other"),
            ("hostinfo.py does not compile", {"hostagent/install.sh": FAKE_INSTALL, "hostagent/hostinfo.py": "def (:\n"}),
        ]
        for name, files in cases:
            self.installed_now(OLD)
            self.latest_is(NEW)
            if os.path.exists(self.env["FAKE_TGZ"]):
                os.remove(self.env["FAKE_TGZ"])
            if files == "other":
                self.archive(OLD)
            elif files is not None:
                self.archive(NEW, files)
            code, out = self.run_updater()
            self.assertEqual(code, 1, (name, out))
            self.assertEqual(self.state_is(), OLD, name)
            self.assertFalse(os.path.exists(self.installed), name)

    def test_a_failing_install_keeps_the_old_commit_so_the_next_night_tries_again(self):
        self.installed_now(OLD)
        self.latest_is(NEW)
        self.archive(NEW)
        code, out = self.run_updater("--quiet", FAKE_INSTALL_FAIL="1")
        self.assertEqual(code, 1, out)
        self.assertIn("useradd: broken", out)
        self.assertEqual(self.state_is(), OLD)

    def test_two_runs_do_not_overlap(self):
        self.installed_now(NEW)
        self.latest_is(NEW)
        with open(self.env["HOSTINFO_UPDATE_LOCK"], "w") as lock:
            holder = subprocess.Popen(["flock", self.env["HOSTINFO_UPDATE_LOCK"], "sleep", "5"])
            self.addCleanup(holder.wait)
            self.addCleanup(holder.kill)
            for _ in range(100):                                       # until the other run holds the lock
                if subprocess.run(["flock", "-n", self.env["HOSTINFO_UPDATE_LOCK"], "true"]).returncode != 0:
                    break
                time.sleep(0.05)
            code, out = self.run_updater()
        self.assertEqual(code, 1, out)
        self.assertIn("Another hostinfo-update is running", out)

    def test_unknown_options_and_odd_settings_are_refused(self):
        self.assertEqual(self.run_updater("--now")[0], 2)
        self.assertEqual(self.run_updater(HOSTINFO_REF="main; reboot")[0], 2)
        for repo in ("../x", "a/..", "-x/y", "a/b/c", "a b/c"):
            self.assertEqual(self.run_updater(HOSTINFO_REPO=repo)[0], 2, repo)
        self.assertEqual(self.run_updater(HOSTINFO_REF="../main")[0], 2)


if __name__ == "__main__":
    unittest.main()
