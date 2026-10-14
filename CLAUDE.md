# PDU Monitor: notes for Claude

Read this first in a new session. It is the hand-over of the sessions that built the project. The owner's notes in `README.md`,
`server/README.md` and `hostagent/README.md` describe how things are installed and used; this file says how the project is put
together, what was decided and why, and what is not finished.

## What it is

Monitoring and control for a data centre: power per rack from the PDUs (CyberPower, APC; SNMP v1) and the load of KVM hypervisors
(CPU, memory, ZFS, VMs, the busiest VMs per period). The two are joined by the outlet name of a server (for example `200U31`): the
same name on the two PDUs of a rack is one server, and a host with that `outletKey` shows its power.
On / Off / **Restart** of outlets with a confirmation (restart: all ports of a server off, the PDUs confirm, pause 5-10 s, all on).

Three parts in one repository:

| Part | Where | Language | State |
|---|---|---|---|
| **Server** (web UI with its login, API, PDU polling) | `server/` (module `github.com/kblaskoff/pdu-monitor/server`) | Go | the main product; installed on the owner's server from a clone, the LDAP login works against the real directory; not yet tried against real PDUs |
| **Host agent** (samples a hypervisor once a minute) | `hostagent/` | Python stdlib, must stay 3.6-compatible (AlmaLinux 8 hosts) | running on real hypervisors: ZFS (Debian 13), LVM (AlmaLinux 8); 0.2.0 adds LVM + filesystems, 0.2.2 finds VMs by `/proc` when the libvirt pid files are root-only (0600, seen on a host with Python 3.6) |
| **Collector** (`pdu-collector`, passive reader of hypervisors) | `server/cmd/pdu-collector`, `server/internal/collector` | Go | tested, not tried on real hosts |
| Mac app (the first version) | `Sources/`, `Tests/`, `Package.swift` | Swift / SwiftUI | works and was tested by the owner; superseded by the web server, not removed |

## Architecture and decisions (do not undo without asking the owner)

* **Words (the owner's, use them in answers):** the **central server** is the machine where `pdumonitord` (the web interface) runs. The owner calls the
  hypervisors that run the agent **hosts, and also "collectors"** ("they collect the data, the central one takes it from them"): the machines that are
  read directly (`a40sf`, `kvmsfc7`, ...). The separate program `pdu-collector` (a passive relay between hosts and the central server, built for hosts that the
  central server cannot reach) is a different thing: say "pdu-collector" for it, never "collector" alone, and do not assume it is deployed anywhere
  (it was not confirmed). The things on the hosts are updated by `hostinfo-update` (nightly; `hostinfo-update --force` at once), not by the Software tab.

* **Central Linux server with a web UI** (Ubuntu 22, Go, one binary `pdumonitord`), 5-15 users, internal network and VPN only.
  Chosen over native apps. The Mac app stays for now; the server can import its settings file (Settings -> Import).
* **Login to the web UI: LDAP only, and only the group `admins` may enter.** LDAP is used for nothing else: the collector, the agents
  and the PDUs never touch it. The roles admin / operator / viewer exist
  in the code; today only `admin: [admins]` is configured. The directory is an old version: filters, group lookup, StartTLS/LDAPS and `legacy_tls` are
  configurable, and `pdumonitord ldap-test` shows every step. No TOTP (VPN + LDAP is enough). An emergency local admin (bcrypt) is optional.
* **PDUs are always read by the server** (SNMP v1; communities are encrypted in the database with `secret.key`).
* **Hypervisors: no SNMP.** The agent (`hostagent/hostinfo.py`) runs from a systemd timer as the account `hostinfo` (root only where `/proc` has `hidepid` or `zpool` needs it), writes plain files; a read-only
  user (`hostinfo-ro`) with a forced-command SSH key can only run `status` and `top 1h|6h|24h|7d`. Format: `hostagent/FORMAT.md` (format 1).
  Storage: ZFS pools (`zpool`/`zfs`), LVM volume groups (`vgs`/`lvs` need root: a separate root timer `hostinfo-lvm` writes `lvm.json`), mounted filesystems (statvfs).
  The key line has `from="ADDRESS"`: it must be the address the host *sees* (NAT!); sshd logs "correct key but not from a permitted host" otherwise.
  Updates: `hostinfo-update` (`hostagent/update.sh`, a root timer every night) asks GitHub for the last commit of `hostagent/` on
  `main` and installs the archive of that commit with `install.sh --update` only when it differs from `/var/lib/hostinfo/installed-commit`.
  The owner decided against a release branch: every agent change is tried by hand first, so what reaches `main` in `hostagent/` goes to every host the next night.
  Reading is done either by the server itself over SSH, or by a **collector**.
* **Host keys are pinned.** A person compares the fingerprint with the one on the host and types it (web UI for the server, `pdu-collector trust`
  for the collector). There is no silent trust-on-first-use.
* **Collector = passive:** no web, no LDAP, no listening port. It reads agents and POSTs to `/api/collector/report` with a bearer token
  (the server keeps only its SHA-256 in `config.yaml`: `pdumonitord collector-token -name NAME`). It pushes statuses every 60 s and the tops every 300 s.
  Hosts a collector reports are created on the server by themselves (`hosts.collector` column). The owner said the PDU collector must
  not be a separate server: PDUs stay in `pdumonitord`.
* **Outlet names come from servman and are written to the PDUs** (the owner's decision: the PDU itself carries the name; the system reads the names back).
  `server/internal/servman` (client of the inventory API, token from a file) + planner; `app/servman.go` (plan, write with read-back check, audit, loop);
  the intervals (ask / write, 0 = off) and the form (`200U13` or `200U13 a42sf`) are **settings in the web UI**, not in the config file.
  Safety rules: only outlets servman names are touched, conflicts (two devices on one outlet) are never written **and are shown to everybody at the top of the overview, one per line, until servman is
  fixed** (the owner: "conflicts must not exist"), the automatic write is off by default (no cap on how many outlets one round writes: the owner decided so),
  a SET without an answer is repeated and a lost answer is recognised by reading the PDU back, everything is audited. `pdu.ServerKey` takes the first token when the name is `CCID + machine name`. The writable name OIDs
  (Config tables) are from the MIB documentation and not yet proven on every real model. Servman API: `/api/servers/get?per_page=all`, each
  record has `ccid` (200U13) and `pdu: [{pdu_ccid, pdu_port}]`; the owner's real token must never be written anywhere in the repo.
* **CPU idle history** (host page, under "Busiest VMs": 14 days as daily bars, 1 year as weekly bars): the server keeps hourly aggregates
  (sum, readings, lowest) of the idle % it already reads every minute in table `host_idle` (`store/idle.go`, migration 3; `app/idle.go` keeps the
  current hour in memory, flushes every 5 min and on shutdown; 400 days of retention). No extra load on the hosts, about 8,760 rows per host and year.
  It fills from the day this was installed (no backfill). Days/weeks are cut in the time zone of the viewer's browser. API `GET /api/hosts/{id}/idle?range=14d|1y&tz=`.
* **Updates from the web (Settings → Software)** (tried on the real central server: works; the table of the agent versions of all hosts is on the same tab) (the owner's decisions: built locally from a clone, by a button not by a timer, no SSH, a minute or two of delay is fine):
  the version is the git commit (`internal/buildinfo`, from `debug.ReadBuildInfo`; no ldflags). The application never runs anything: it writes
  `update-request` into its data dir (`internal/updater`), and a **root** systemd path unit starts `deploy/update.sh` (installed as
  `/usr/local/sbin/pdu-update server|collector`: git fetch + ff-only, go build to a temp dir, skip if same version, replace binary, restart,
  roll back when it does not stay up; writes `update.status`/`update.log` that the page shows). Collectors: the server holds a flag per
  collector and returns `"update": id` in the answer to `/api/collector/report`; the collector writes its own request file and acknowledges with
  `update_seen`; the flag is dropped on ack, on a version change or after 15 minutes. `deploy/install.sh` and `deploy/collector/install.sh` install the
  units and `update.conf` (REPO_DIR, BRANCH) and the `.updater` marker. Needs git + go (also Go from snap: `/snap/bin` is in the script's PATH) + a fast-forwardable clone on each machine; git runs as the owner of the clone (so his next `git pull` works), the build as root. The script is
  tested here with a fake `systemctl` only; the units are not tried on a real host. The host agents keep the nightly `hostinfo-update`.
* **No notifications for ZFS DEGRADED** (the owner has Nagios). Over-limit alerts for racks: channel not decided yet.
* The owner asked that more host statistics will come later; keep the agent format extensible (versioned, strict parsing on the server side).

## Where things are (server)

```
server/cmd/pdumonitord     the daemon: serve, init, ldap-test, pubkey, collector-token, hash-password, selfsigned, version
server/cmd/pdu-collector   run, once, init, pubkey, trust, version
server/internal/snmp       BER/SNMP v1 client + a fake agent for tests (snmptest)
server/internal/pdu        drivers (cyberpower.go, apc.go), model, rack arithmetic, restart sequencer, simulated PDUs
server/internal/store      SQLite (modernc, pure Go): racks, pdus, hosts, labels, audit, settings, samples; migrations in store.go
server/internal/hostinfo   agent output parsing (strict), SSH client with pinned keys, the poller/cache
server/internal/collector  config, known_hosts, the push loop
server/internal/auth       LDAP directory, local admin, sessions, rate limiting
server/internal/app        PDU polling, power operations with audit and busy-locking
server/internal/web        HTTP API and handlers; static/ is the single-page UI (vanilla JS, no framework, no build step)
server/internal/demo       demo data: 4 racks, 8 PDUs, 15 hosts (used by `serve -demo` and by tests)
server/deploy              systemd unit + install.sh (server), deploy/collector for the collector
```

UI: `server/internal/web/static/{index.html,style.css,app.js}`, embedded with `go:embed`. Text is always added as text (`h()` helper),
never as HTML. Hash routing. Light and dark themes.

## Commands

```
cd server
gofmt -l .                     # must print nothing
go vet ./...
go test -race ./...            # about a minute; CI runs the same
CGO_ENABLED=0 go build -o pdumonitord ./cmd/pdumonitord
./pdumonitord serve -demo -listen 127.0.0.1:8080     # demo / demo (admin), operator / operator, viewer / viewer

python3 -m unittest discover -s hostagent/tests -v    # host agent (CI uses Python 3.9 and 3.12)
swift test                                            # Mac core (Linux ok); the Mac app itself builds only on macOS in CI
```

CI: `.github/workflows/macos.yml` (named "build"): jobs `core-linux`, `hostagent`, `server` (vet, race tests, builds `pdumonitord` and
`pdu-collector` as the artifact `pdu-monitor-server`) and `app` (macOS, signed only if secrets exist).

## How to work here

* The owner **does not write code and does not use Xcode or a Go toolchain**: everything is built by CI and delivered as an artifact.
  Explain in plain steps, and in Bulgarian when the owner writes in Bulgarian. The code, comments and UI text are English.
* Verify before saying it works: write tests next to the code, do a **mutation check** on security-relevant tests (break the code,
  see the test fail, restore), run `-race`. For UI changes run the demo server and take screenshots with Playwright + Chromium
  (`/opt/pw-browsers/chromium`) in light and dark, and check for console errors. Look at the screenshots.
* Be honest about what is untested: LDAP against the real directory, real PDUs, real hypervisors, install scripts and systemd units on a real host.
* Comments say why, not what; match the surrounding style. Errors shown to people are written for people ("the key of this host is not confirmed yet").
* Security stance: the web server is internal only, but treat it as hostile: CSRF header + Origin/Sec-Fetch-Site checks, CSP, rate limiting
  of logins and tokens, no secret in the log or the audit, strict parsing of everything that comes from hosts or collectors.
* **Secrets never go into the repository**: SNMP communities, LDAP bind password, collector tokens, SSH keys, `secret.key`, TLS keys.
  The repository is **public**: no real domain, host name or directory DN of the owner either; examples use `replace_me`.
* Git: **every session (and every person) works in its own branch** (`claude/...`, the one the session is given); two sessions never
  push to the same branch. Rule agreed with the owner: **after CI is green on the branch, the work goes into `main`**.
  Before that, bring `main` into your branch (`git fetch origin main && git merge origin/main`), resolve conflicts, run the tests again, push the
  branch, wait for green CI, then update `main` (`git push origin <branch>:main`, a fast-forward). If `main` moved meanwhile and the push is
  rejected, merge `origin/main` again and repeat. **Never force-push** and never rewrite `main`. Do not open pull requests unless asked.
  The GitHub tools are `mcp__github__*` (no `gh` CLI in the cloud sessions).

## Not done yet / ideas

* Graphs of PDU history (the samples are already stored in `pdu_samples`) and of more host history (idle bars exist; memory/load/disk do not).
* Alerts for racks over the limit (channel to decide); more host statistics (the owner will ask).
* Try everything against the real LDAP (`ldap-test`), real PDUs and a real hypervisor; adjust the example config for the real directory
  (needs from the owner: URL, OpenLDAP or AD, `user_base`, `group_base`, TLS/CA).
* Mac app: "Save does not keep the PDU" was reported once and never reproduced; the restart window glitch with a read-only PDU was accepted as a small bug;
  update feed (Sparkle) hosting and signing/notarization secrets are not set up. Retire the app after the web UI has parity, if the owner wants.
* Roles other than `admin` are implemented but not exposed in the LDAP example.
