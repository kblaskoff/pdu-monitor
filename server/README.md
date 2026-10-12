# PDU Monitor server

One Go binary (`pdumonitord`) that serves the web interface, reads the PDUs over SNMP v1, reads the hypervisors over SSH (the agent in
`../hostagent`), keeps a small SQLite database and lets members of the LDAP group `admins` in.

```
browser ──https──▶ pdumonitord ──SNMP v1 (UDP 161)──▶ PDUs   (management network)
 (VPN + LDAP)          │  └──ssh, forced command, pinned host key──▶ hypervisors (user hostinfo-ro)
                       └──LDAP(S)──▶ directory
```

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

## Data and backup

Everything is in `/var/lib/pdu-monitor`: `pdu-monitor.db` (racks, PDUs, hosts, history, log), `secret.key` (encrypts the SNMP
communities in the database) and the SSH key of the server. Back up the whole directory; without `secret.key` the stored
communities cannot be read. Settings of the Mac application can be imported under Settings → Import.

## Build and test

```
cd server && go vet ./... && go test -race ./... && CGO_ENABLED=0 go build -o pdumonitord ./cmd/pdumonitord
```
