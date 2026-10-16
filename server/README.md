# PDU Monitor server

One Go binary (`pdumonitord`) that serves the web interface, reads the PDUs over SNMP v1, reads the hypervisors over SSH (the agent in
`../hostagent`) or through a collector, keeps a small SQLite database and lets members of the LDAP group `admins` in.

```
browser ──https──▶ pdumonitord ──SNMP v1 (UDP 161)──▶ PDUs   (management network)
 (VPN + LDAP)          │  ├──ssh, forced command, pinned host key──▶ hypervisors (user hostinfo-ro)   [read by the server itself]
                       │  └──LDAP(S)──▶ directory
                       ▲
 pdu-collector ──https, token──┘   (outbound only, no web, no LDAP)──ssh──▶ hypervisors  [read by a collector]
```

The PDUs are always read by the server. Hypervisors can be read by the server itself or by a **collector** (`pdu-collector`, a small
separate program for a network where the server does not reach the hypervisors, or where you do not want SSH keys on the web server).
The collector is passive: it listens on no port, has no web interface and does not know about LDAP; it only reads the agents and pushes
the readings to the server over HTTPS with its own token. See [Collector](#collector).

## Try it without any device

```
pdumonitord serve -demo -listen 127.0.0.1:8080        # demo / demo (admin), operator / operator, viewer / viewer
```

Sample racks, PDUs and 15 hypervisors; nothing is sent to any device. Data is kept in memory only.

## Install on Ubuntu 22.04

This is the short version with the program built by CI. To build it on the server from the repository, see
[Install from the repository](#install-from-the-repository): the same steps in full, with the permissions.

1. Get `pdumonitord` from the CI artifact `pdu-monitor-server` (job **server**) and unpack it with the `deploy/` folder.
2. `sudo ./install.sh --binary ./pdumonitord`: creates the user `pdumonitor`, `/etc/pdu-monitor`, `/var/lib/pdu-monitor`, the systemd unit
   and an example `config.yaml`. Running it again never overwrites the configuration, the database or the keys.
3. Edit `/etc/pdu-monitor/config.yaml` (see below), put the TLS certificate in place (from your internal CA, or
   `sudo pdumonitord selfsigned -host NAME -out /etc/pdu-monitor` for a start).
4. Check the directory login **before** starting the service. It prints every step (connect, search, bind, groups):
   `sudo -u pdumonitor pdumonitord ldap-test -config /etc/pdu-monitor/config.yaml -user YOURLOGIN`
5. `sudo systemctl start pdu-monitor`, then `journalctl -u pdu-monitor -f`.
6. Allow the port only from the VPN and the management network: `sudo ufw allow from 10.0.0.0/8 to any port 8443 proto tcp`
   (adjust), and make sure the server can reach the PDUs (UDP 161) and the hypervisors (TCP 22).

## Install from the repository

Everything is in one place: the server, its `deploy/` folder and the host agent. The web interface is built into the program
(there are no web files to copy and no Apache or nginx: `pdumonitord` serves HTTPS itself). In the examples, `replace_me` stands
for your own names.

**1. Go and the repository.** The Go of Ubuntu 22.04 (`apt install golang`) is too old; the snap is current:

```
sudo snap install go --classic
sudo apt install git sqlite3
sudo mkdir /opt/pdu-monitor && sudo chown "$USER" /opt/pdu-monitor
git clone https://github.com/kblaskoff/pdu-monitor.git /opt/pdu-monitor
```

The clone belongs to you, not to root: building needs no `sudo` (and git refuses to work in a folder owned by another user).

**2. Build and install.** The tests are the same as in CI and take about a minute.

```
cd /opt/pdu-monitor/server
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o pdumonitord ./cmd/pdumonitord
sudo ./deploy/install.sh --binary ./pdumonitord
```

`install.sh` creates the user `pdumonitor`, `/usr/local/bin/pdumonitord`, the systemd unit, `/etc/pdu-monitor/config.yaml` and
`/var/lib/pdu-monitor`. Running it again never overwrites the configuration, the database or the keys.

**3. The certificate.** `tls.crt` is the certificate of the server followed by the intermediate certificates (the full chain), in PEM;
`tls.key` is its private key in PEM, without a passphrase. A wildcard certificate is fine. The service runs as `pdumonitor`, so the
group must be able to read them:

```
sudo install -m 644 -o root -g pdumonitor fullchain.pem /etc/pdu-monitor/tls.crt
sudo install -m 640 -o root -g pdumonitor privkey.pem   /etc/pdu-monitor/tls.key
```

A renewed certificate is copied the same way; the server reads it again within a minute, without a restart.

**4. The directory password.** One line, the password of the account in `ldap.bind_dn`:

```
sudo sh -c 'umask 027; printf "%s" "THE-PASSWORD" > /etc/pdu-monitor/ldap-bind-password'
sudo chown root:pdumonitor /etc/pdu-monitor/ldap-bind-password
sudo chmod 640 /etc/pdu-monitor/ldap-bind-password
```

**5. The configuration.** Edit `/etc/pdu-monitor/config.yaml` and replace every `replace_me` (see
[Directory (LDAP) login](#directory-ldap-login)). Keep `listen` as `":8443"` or an IP address; the name people type
(`pdumonitor.replace_me`) only has to be in the DNS and in the certificate. Port 443 instead of 8443: remove the `#` before
`AmbientCapabilities=CAP_NET_BIND_SERVICE` in `/etc/systemd/system/pdu-monitor.service`, then `sudo systemctl daemon-reload`.

**6. Check the login, then start.** Always run the program as `pdumonitor`, never as root: files that root creates in
`/var/lib/pdu-monitor` cannot be opened by the service afterwards.

```
sudo -u pdumonitor pdumonitord ldap-test -config /etc/pdu-monitor/config.yaml -user YOURLOGIN
sudo systemctl start pdu-monitor
sudo systemctl status pdu-monitor
journalctl -u pdu-monitor -f
```

`ldap-test` should end with `role "admin"`. If it lists your groups but the role is empty, `ldap.roles` names a group you are not in.
Then open `https://pdumonitor.replace_me:8443`. The log line "ConfigurationDirectory 'pdu-monitor' already exists but the mode is
different" is harmless: `/etc/pdu-monitor` is 750 on purpose.

**If the service does not start** but works when started by hand as root, the permissions are wrong. `journalctl -u pdu-monitor -n 20`
names the file. Put them right with:

```
sudo chown -R pdumonitor:pdumonitor /var/lib/pdu-monitor
sudo chown root:pdumonitor /etc/pdu-monitor/config.yaml /etc/pdu-monitor/tls.crt /etc/pdu-monitor/tls.key /etc/pdu-monitor/ldap-bind-password
sudo chmod 640 /etc/pdu-monitor/config.yaml /etc/pdu-monitor/tls.key /etc/pdu-monitor/ldap-bind-password
sudo systemctl restart pdu-monitor
```

**7. Hypervisors.** Print the key of this server; each hypervisor takes the agent from the archive on GitHub and installs it with
that key. The full steps (checks, confirming the host key, update, removal) are in [../hostagent/README.md](../hostagent/README.md):

```
sudo -u pdumonitor pdumonitord pubkey -config /etc/pdu-monitor/config.yaml
```

**Update.** The configuration, the database and the keys stay as they are:

```
cd /opt/pdu-monitor && git pull
cd server && go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(git rev-parse --short HEAD)" -o pdumonitord ./cmd/pdumonitord
sudo ./deploy/install.sh --binary ./pdumonitord
sudo systemctl restart pdu-monitor
```

`journalctl -u pdu-monitor` shows the new version (the commit) on the line "listening on".

## Directory (LDAP) login

Only people who belong to a group named in `ldap.roles` can log in. A wrong user or password gets "wrong user or password"; a
right password of somebody outside the groups gets "this account is not in a group that may use this application". Roles: `admin` (everything), `operator` (switch outlets, rename), `viewer` (look). Several groups can be named, one list per role:
`roles:` with `admin: [admins]`, `operator: [group-a]`, `viewer: [group-b]` (a person in more than one group gets the highest role).

* OpenLDAP: `user_filter: "(uid={username})"`, set `group_base` (groups with `member`, `uniqueMember` or `memberUid`), or `member_of_attr: memberOf` if
  the memberof overlay is on.
* Active Directory: `user_filter: "(&(objectClass=user)(sAMAccountName={username}))"`, `member_of_attr: memberOf`.
* Old directory servers: if the connection fails with a protocol/handshake error, add `legacy_tls: true` (allows TLS 1.0/1.1 and old
  ciphers for the directory connection only). Prefer `ldaps://` or `start_tls: true`: with plain `ldap://` the password travels in clear.
* If the directory does not allow a search before the login, use `user_dn_template` instead of the search.
* `debug: true` adds the steps of every login (never the password) to the log.

The emergency account (`local_admin`) works without the directory; make its hash with `pdumonitord hash-password`. Leave it out if
you do not want one.

## Second step of the login, passkeys, how long a login lasts

After the password a person can be asked for a second step (the top right, under the name: **Your login**):

* **Authenticator apps** (6-digit codes: Google Authenticator, Microsoft Authenticator, Authy, 1Password…): up to **3 devices** per person, each with its own
  key (so a lost phone is removed alone). The QR code is drawn by the page itself; nothing is sent to anybody else. A code is accepted once.
* **Passkeys** (up to 3 per person): the fingerprint reader of a laptop (Touch ID, Windows Hello), a phone, a security key. The browser asks for the
  fingerprint after the password; the fingerprint (or the PIN) must have been checked, or the passkey is refused. Passkeys are tied to the *name*
  of the site and need https, so add the address people type to `config.yaml` and restart (a host name, not an IP address):
  `public_url: https://pdu.example.com:8443`. Without it the authenticator apps work and the page says why passkeys are off.
* A password manager that does not answer the browser (seen with LastPass) leaves the question open; the page lets you ask again or use a code, and the
  iCloud / laptop passkeys or an authenticator app keep working. Passkeys made by a password manager are only as good as its extension: use one that works.
* **Recovery codes** (10, made with the first device, shown once, each works once): the way in when a phone is lost.
* **Settings → Settings → Login:** *Second step for everybody* (anybody without one must add one right after the password and sees nothing else until
  then; the emergency local account is not forced), and *Stay logged in (hours)*, default **72** (three days), applies at once. Sessions are kept in the
  database, so an update or a restart of the server does not log anybody out.
* **Settings → Users** lists who has a second step and removes it for somebody who lost every device and every recovery code (they are logged out everywhere).
  When no administrator can log in: `sudo -u pdumonitor pdumonitord mfa-list -config /etc/pdu-monitor/config.yaml` and
  `… mfa-reset -config … -user ldap:NAME`, then `systemctl restart pdu-monitor`.
* Wrong codes are limited: five for one password check, eight in 15 minutes for one person.

## Hypervisors

Settings → Hosts shows the install command with the key of this server (the same as `pdumonitord pubkey`). Install the agent on a host
(`hostagent/install.sh --key '…'`), add the host in the web interface, press **Check the key**, compare the fingerprint with
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the host and paste it to confirm. Without that confirmation nothing is read:
this is what stops somebody on the network from pretending to be a hypervisor.

The page of a host shows **CPU idle %** per day for the last 14 days and per week for the last year. The server builds it from the readings
it already takes (hourly averages in its database, about 9,000 small rows per host and year, kept 400 days), so it costs the hosts nothing
and the history starts on the day the server was updated.

## Collector

`pdu-collector` runs where the hypervisors can be reached (it can also run on the same machine as the server).

1. On the server: `pdumonitord collector-token -name collector-a`. It prints the token (shown once; the server keeps only its SHA-256)
   and the `collectors:` lines for the server's `config.yaml`. Add them and restart the server (`Settings → Hosts` then lists the collector).
2. On the collector machine: `sudo ./deploy/collector/install.sh --binary ./pdu-collector`, put the token into `/etc/pdu-collector/token`
   (mode 640) and edit `/etc/pdu-collector/config.yaml` (the server address and the list of hypervisors).
3. `sudo -u pdu-collector pdu-collector pubkey -config /etc/pdu-collector/config.yaml` prints the key of the collector; install the agent on
   each hypervisor with it (`hostagent/install.sh --key '…'`).
4. `sudo -u pdu-collector pdu-collector trust -config /etc/pdu-collector/config.yaml -all` shows the SSH key of each hypervisor and asks
   you to type the fingerprint that `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` prints on the host. Nothing is read without it.
5. `pdu-collector once -config …` tries one reading, then `sudo systemctl start pdu-collector`.

Hosts that the collector reports appear on the server by themselves (**Settings → Hosts**, "via collector-a"): link each one to a rack
and its power outlet name with **Edit** to join it with its power. The server needs no address or key for them.

* The status is pushed every `poll_seconds` (60), the busiest VMs of the four periods every `top_seconds` (300). If the server is
  unreachable the collector just tries again with fresh readings: the agents keep their own history, nothing is lost.
* The token is the only secret that crosses the network: use `https` (the collector refuses plain http except for localhost; it never
  follows redirects, so the token cannot be sent anywhere else). For your own CA set `ca_file`.
* A name that the server reads itself (or another collector reads) is refused. Wrong tokens lock the sender's address for a while.

## Updating from the web interface (Settings → Software)

The version of the server and of every collector is the commit they were built from. **Update the server** and **Update all collectors**
(or one; the buttons for the `pdu-collector` relays) build the newest `main` of the git clone that is on the machine and restart the program. Nothing connects anywhere with SSH:

* **The server:** the web process writes one file (`/var/lib/pdu-monitor/update-request`). A root systemd path unit
  (`pdu-monitor-update.path`) sees it and runs `/usr/local/sbin/pdu-update server` (`deploy/update.sh`): `git fetch` and fast-forward of the
  clone, `go build` into a temporary directory, nothing to do when the commit is the one that runs, otherwise it replaces
  `/usr/local/bin/pdumonitord`, restarts the service and **puts the old binary back** if the new one has not stayed up for 15 seconds.
  The state and the output are shown on the page; the web interface is away for about half a minute.
* **The hosts** are not updated from here: their agents update themselves every night (`hostinfo-update`). The page lists the agent version of every host and marks
  the ones that are older than the newest one reported.
* **The collectors:** the button only sets a flag on the server. Every collector hears it in the answer to its next report (within a minute),
  leaves its own request file, and its own root path unit does the same as above. After the restart it reports its new version; a
  request that nobody takes is dropped after 15 minutes.

Once on the server and once on every collector, to put the root side in place (re-running the install script is safe):
`sudo ./deploy/install.sh --binary ./pdumonitord` and `sudo ./deploy/collector/install.sh --binary ./pdu-collector`, from the git clone.
They write `REPO_DIR=` (the clone) and `BRANCH=main` into `/etc/pdu-monitor/update.conf` (`/etc/pdu-collector/update.conf`). The machine
needs `git` and `go` (also Go from snap), and a clone that can be fast-forwarded: no local commits or changes in it. `git` runs as the owner of the clone, so a later `git pull` by that user works. `journalctl -u pdu-monitor-update` has the details.

## Names of the outlets (servman)

The servers are joined with their power by the **name of the outlet** (`200U13`: rack 200, unit 13). Servman, the inventory, knows which
server is plugged into which outlet of which PDU; `pdumonitord` compares that with the names on the PDUs and can write the right names
to them. The names are then read back from the PDUs like any other name, so the rest of the system needs nothing more.

**Connect it** (once, in `/etc/pdu-monitor/config.yaml`; leave the section out to turn it off):

```yaml
servman:
  url: "https://replace_me.example.com"
  token_file: /etc/pdu-monitor/servman-token     # the token of the API, one line
  ca_file: /etc/pdu-monitor/servman-ca.pem       # the CA of its certificate; or, if you cannot get it: insecure_skip_verify: true
  # sources: [servers]                           # add netswitches to name the network devices (N200U43) from their list too
```

The token is a secret: `sudo install -m 640 -o root -g pdumonitor /dev/stdin /etc/pdu-monitor/servman-token` and paste it, or write the
file and `chmod 640`. It is never in the repository, in the configuration file or in the log. Restart the service.

**Look at it** under *Settings → Names*: *Compare now* asks servman and shows, per PDU, which outlets would get which name. Nothing is
written by that. *new* means the outlet still has the default name (`Outlet_7`), *rename* that another name is replaced. Things that do not
fit are listed and left alone: two servers on one outlet (a mistake in servman), a PDU that servman has but this system does not, an outlet
number that the PDU does not have, a PDU that was not read. Outlets that have a name of their own that servman does not know are only listed.

**Write it** with *Write all*, *Write 3* (one PDU) or *Write* (one outlet). Start with one outlet and look at the PDU. Every writing needs
a confirmation, runs in the background, reads the PDU again and compares: a PDU that says "ok" and still shows the old name is reported.
The log (*Settings → Log*) has a line per PDU: who, which outlets, from what to what. A PDU without a write community is shown but not written.

**By itself** (*Settings → Settings*): *Ask servman every … min* (default 15, 0 = never) makes the plan; *Write to the PDUs every … min*
(default **0 = off**) writes it, everything that differs, as many outlets as there are (the first round of a new installation writes it all).
*Outlet name* is `200U13` or `200U13 a42sf` (the machine name; cut to fit the 20 characters of an APC outlet name). With
both forms the two PDUs of a server are one line and a host is joined by the id: the id alone identifies the server.

What is written is the *Config* name object of the MIB (read-write; the object that is read is a read-only copy): CyberPower ePDU2
(`ePDU2OutletSwitched/Metered/MonitoredConfigName`), APC rPDU2 (`rPDU2OutletSwitched/MeteredConfigName`) and the older APC rPDU
(`rPDUOutletConfigOutletName`). The older CyberPower "SW" models are not written to. These object numbers come from the MIB files and
are covered by tests against a simulated device; they have **not** been tried on a real PDU of every model: that is why the read-back
check exists. Names are ASCII, 1 to 20 characters.

## Data and backup

Everything is in `/var/lib/pdu-monitor`: `pdu-monitor.db` (racks, PDUs, hosts, history, log), `secret.key` (encrypts the SNMP
communities in the database) and the SSH key of the server. Back up the whole directory; without `secret.key` the stored
communities cannot be read. Settings of the Mac application can be imported under Settings → Import.

## Build and test

```
cd server && go vet ./... && go test -race ./... && CGO_ENABLED=0 go build -o pdumonitord ./cmd/pdumonitord
```
