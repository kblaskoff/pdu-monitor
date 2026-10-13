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
right password of somebody outside the groups gets "this account is not in a group that may use this application". Roles: `admin` (everything), `operator` (switch outlets, rename), `viewer` (look). The decision is "admins only" for now:
only `admin: [admins]` is configured.

* OpenLDAP: `user_filter: "(uid={username})"`, set `group_base` (groups with `member`, `uniqueMember` or `memberUid`), or `member_of_attr: memberOf` if
  the memberof overlay is on.
* Active Directory: `user_filter: "(&(objectClass=user)(sAMAccountName={username}))"`, `member_of_attr: memberOf`.
* Old directory servers: if the connection fails with a protocol/handshake error, add `legacy_tls: true` (allows TLS 1.0/1.1 and old
  ciphers for the directory connection only). Prefer `ldaps://` or `start_tls: true`: with plain `ldap://` the password travels in clear.
* If the directory does not allow a search before the login, use `user_dn_template` instead of the search.
* `debug: true` adds the steps of every login (never the password) to the log.

The emergency account (`local_admin`) works without the directory; make its hash with `pdumonitord hash-password`. Leave it out if
you do not want one.

## Hypervisors

Settings → Hosts shows the install command with the key of this server (the same as `pdumonitord pubkey`). Install the agent on a host
(`hostagent/install.sh --key '…'`), add the host in the web interface, press **Check the key**, compare the fingerprint with
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the host and paste it to confirm. Without that confirmation nothing is read:
this is what stops somebody on the network from pretending to be a hypervisor.

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

## Data and backup

Everything is in `/var/lib/pdu-monitor`: `pdu-monitor.db` (racks, PDUs, hosts, history, log), `secret.key` (encrypts the SNMP
communities in the database) and the SSH key of the server. Back up the whole directory; without `secret.key` the stored
communities cannot be read. Settings of the Mac application can be imported under Settings → Import.

## Build and test

```
cd server && go vet ./... && go test -race ./... && CGO_ENABLED=0 go build -o pdumonitord ./cmd/pdumonitord
```
