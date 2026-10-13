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
| **Host agent** (samples a hypervisor once a minute) | `hostagent/` | Python stdlib, must stay 3.6-compatible (AlmaLinux 8 hosts) | running on real hypervisors: ZFS (Debian 13), LVM (AlmaLinux 8); agent 0.2.0 adds LVM + filesystems |
| **Collector** (`pdu-collector`, passive reader of hypervisors) | `server/cmd/pdu-collector`, `server/internal/collector` | Go | tested, not tried on real hosts |
| Mac app (the first version) | `Sources/`, `Tests/`, `Package.swift` | Swift / SwiftUI | works and was tested by the owner; superseded by the web server, not removed |

## Architecture and decisions (do not undo without asking the owner)

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
  Reading is done either by the server itself over SSH, or by a **collector**.
* **Host keys are pinned.** A person compares the fingerprint with the one on the host and types it (web UI for the server, `pdu-collector trust`
  for the collector). There is no silent trust-on-first-use.
* **Collector = passive:** no web, no LDAP, no listening port. It reads agents and POSTs to `/api/collector/report` with a bearer token
  (the server keeps only its SHA-256 in `config.yaml`: `pdumonitord collector-token -name NAME`). It pushes statuses every 60 s and the tops every 300 s.
  Hosts a collector reports are created on the server by themselves (`hosts.collector` column). The owner said the PDU collector must
  not be a separate server: PDUs stay in `pdumonitord`.
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

* Graphs of PDU history (the samples are already stored in `pdu_samples`) and of host history.
* Alerts for racks over the limit (channel to decide); more host statistics (the owner will ask).
* Try everything against the real LDAP (`ldap-test`), real PDUs and a real hypervisor; adjust the example config for the real directory
  (needs from the owner: URL, OpenLDAP or AD, `user_base`, `group_base`, TLS/CA).
* Mac app: "Save does not keep the PDU" was reported once and never reproduced; the restart window glitch with a read-only PDU was accepted as a small bug;
  update feed (Sparkle) hosting and signing/notarization secrets are not set up. Retire the app after the web UI has parity, if the owner wants.
* Roles other than `admin` are implemented but not exposed in the LDAP example.
