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

1. Get `pdumonitord-linux-amd64` from the CI artifact `pdu-monitor-server` (job **server**) and unpack it with the `deploy/` folder.
2. `sudo ./install.sh --binary ./pdumonitord`: creates the user `pdumonitor`, `/etc/pdu-monitor`, `/var/lib/pdu-monitor`, the systemd unit
   and an example `config.yaml`. Running it again never overwrites the configuration, the database or the keys.
3. Edit `/etc/pdu-monitor/config.yaml` (see below), put the TLS certificate in place (from your internal CA, or
   `sudo pdumonitord selfsigned -host NAME -out /etc/pdu-monitor` for a start).
4. Check the directory login **before** starting the service. It prints every step (connect, search, bind, groups):
   `sudo -u pdumonitor pdumonitord ldap-test -config /etc/pdu-monitor/config.yaml -user YOURLOGIN`
5. `sudo systemctl start pdu-monitor`, then `journalctl -u pdu-monitor -f`.
6. Allow the port only from the VPN and the management network: `sudo ufw allow from 10.0.0.0/8 to any port 8443 proto tcp`
   (adjust), and make sure the server can reach the PDUs (UDP 161) and the hypervisors (TCP 22).

## Directory (LDAP) login

Only people who belong to a group named in `ldap.roles` can log in; everybody else gets the same "wrong user or password" as a wrong
password. Roles: `admin` (everything), `operator` (switch outlets, rename), `viewer` (look). The decision is "admins only" for now:
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
