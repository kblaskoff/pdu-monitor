package hostinfo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

const statusJSON = `{"v":1,"host":"a52sf","t":1760000000,"interval":60,"cpu":{"cores":256,"idle_pct":61.2,"iowait_pct":0.1,"load":[96.1,95.5,94.0]},
"mem":{"total":1081000000000,"available":480000000000,"used":601000000000,"swap_used":0,"zfs_arc":120000000000},
"storage":{"t":1760000000,"zfs":true,"pools":[{"name":"pool","size":91534343012352,"alloc":59918395113472,"free":31615947898880,"cap_pct":65,"health":"ONLINE","usable_used":53246036254320,"usable_avail":27960521023888,"usable_total":81206557278208}]},
"vms":{"count":2,"list":[{"name":"altar22","pid":4321,"rss":8589934592,"cores":1.25},{"name":"mail","pid":4400,"rss":1,"cores":null}]},
"other_cores":0.4,"agent":{"version":"0.1.0","python":"3.9.25"},"errors":[]}`

const topJSON = `{"v":1,"host":"a52sf","window":"6h","t":1760000000,"cores":256,"covered_s":21540.0,"coverage":0.998,"samples":359,"other_cpu_s":5400.2,
"vms":[{"name":"altar22","cpu_s":51234.5,"avg_cores":2.38,"avg_pct":0.93,"peak_cores":8.1,"peak_t":1759990000}]}`

// ---- an SSH server like the one of a hypervisor: key login only, only the fixed commands

type fakeHost struct {
	listener  net.Listener
	hostKey   ssh.Signer
	clientKey ssh.PublicKey
	answers   map[string]string
	conns     atomic.Int32
	mu        sync.Mutex
	open      []net.Conn
}

func newFakeHost(t *testing.T, clientKey ssh.PublicKey) *fakeHost {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ := ssh.NewSignerFromKey(priv)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{listener: l, hostKey: hostKey, clientKey: clientKey, answers: map[string]string{"status": statusJSON, "top 6h": topJSON}}
	t.Cleanup(func() { l.Close(); f.dropAll() })
	go f.serve()
	return f
}

func (f *fakeHost) port() int { return f.listener.Addr().(*net.TCPAddr).Port }

func (f *fakeHost) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.open {
		c.Close()
	}
	f.open = nil
}

func (f *fakeHost) serve() {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) == string(f.clientKey.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("unknown key")
	}}
	cfg.AddHostKey(f.hostKey)
	for {
		raw, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.open = append(f.open, raw)
		f.mu.Unlock()
		go func() {
			conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
			if err != nil {
				return
			}
			f.conns.Add(1)
			go ssh.DiscardRequests(reqs)
			for ch := range chans {
				if ch.ChannelType() != "session" {
					ch.Reject(ssh.UnknownChannelType, "no")
					continue
				}
				channel, requests, _ := ch.Accept()
				go func() {
					for req := range requests {
						if req.Type != "exec" || len(req.Payload) < 4 {
							req.Reply(false, nil)
							continue
						}
						command := string(req.Payload[4 : 4+binary.BigEndian.Uint32(req.Payload)])
						req.Reply(true, nil)
						answer, ok := f.answers[command]
						status := uint32(0)
						if !ok {
							answer, status = `{"error": "command not allowed"}`, 2
						}
						channel.Write([]byte(answer + "\n"))
						channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						channel.Close()
					}
				}()
			}
			conn.Close()
		}()
	}
}

func newClient(t *testing.T) (*SSHClient, ssh.Signer) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	c := NewSSHClient(signer)
	c.Timeout = 3 * time.Second
	t.Cleanup(c.Close)
	return c, signer
}

func hostOf(f *fakeHost, trusted bool) store.Host {
	h := store.Host{ID: 1, Name: "a52sf", Address: "127.0.0.1", Port: f.port(), User: "hostinfo-ro", Enabled: true}
	if trusted {
		h.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(f.hostKey.PublicKey())))
	}
	return h
}

func TestRunAnswersTheFixedCommandsOnly(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	out, err := c.Run(context.Background(), hostOf(f, true), "status")
	if err != nil {
		t.Fatal(err)
	}
	status, err := ParseStatus(out)
	if err != nil {
		t.Fatal(err)
	}
	if status.Host != "a52sf" || *status.CPU.Cores != 256 || len(status.VMs.List) != 2 || status.Storage.Pools[0].Health != "ONLINE" {
		t.Fatalf("%+v", status)
	}
	// a command the host does not allow: the JSON error is returned (exit status 2 is not a failure of the connection)
	out, err = c.Run(context.Background(), hostOf(f, true), "cat /etc/shadow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStatus(out); err == nil || !strings.Contains(err.Error(), "command not allowed") {
		t.Fatalf("%v", err)
	}
}

func TestAHostWhoseKeyIsNotConfirmedIsNotContacted(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	_, err := c.Run(context.Background(), hostOf(f, false), "status")
	if !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("%v", err)
	}
	if f.conns.Load() != 0 {
		t.Fatal("the server connected to a host it does not trust")
	}
}

func TestADifferentHostKeyIsRefused(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	h := hostOf(f, true)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	h.HostKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherSigner.PublicKey())))
	_, err := c.Run(context.Background(), h, "status")
	if err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("%v", err)
	}
	if f.conns.Load() != 0 {
		t.Fatal("the login must not be completed with an unknown key")
	}
}

func TestFetchHostKeyReturnsWhatTheHostOffers(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	key, err := c.FetchHostKey(context.Background(), "127.0.0.1", f.port())
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(key) != ssh.FingerprintSHA256(f.hostKey.PublicKey()) {
		t.Fatal("wrong key")
	}
	if f.conns.Load() != 0 {
		t.Fatal("no login must happen while the key is only looked at")
	}
	if _, err := c.FetchHostKey(context.Background(), "127.0.0.1", 1); err == nil {
		t.Fatal("nothing listens there")
	}
}

func TestTheKeyOfThisServerIsRefusedByTheHost(t *testing.T) {
	c, _ := newClient(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	f := newFakeHost(t, other.PublicKey())
	_, err := c.Run(context.Background(), hostOf(f, true), "status")
	if err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Fatalf("%v", err)
	}
}

func TestTheConnectionIsReusedAndRebuiltAfterABreak(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	h := hostOf(f, true)
	for i := 0; i < 3; i++ {
		if _, err := c.Run(context.Background(), h, "status"); err != nil {
			t.Fatal(err)
		}
	}
	if f.conns.Load() != 1 {
		t.Fatalf("%d connections for 3 requests", f.conns.Load())
	}
	f.dropAll() // the host restarted its sshd
	time.Sleep(100 * time.Millisecond)
	if _, err := c.Run(context.Background(), h, "status"); err != nil {
		t.Fatalf("after the break: %v", err)
	}
	if f.conns.Load() != 2 {
		t.Fatalf("%d connections", f.conns.Load())
	}
}

func TestAnswerThatIsTooBigIsCut(t *testing.T) {
	c, signer := newClient(t)
	f := newFakeHost(t, signer.PublicKey())
	f.answers["status"] = strings.Repeat("x", MaxAnswer+10)
	if _, err := c.Run(context.Background(), hostOf(f, true), "status"); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Fatalf("%v", err)
	}
}

func TestKeyFileIsCreatedOnceAndTheLineIsForAuthorizedKeys(t *testing.T) {
	path := t.TempDir() + "/id"
	a, err := LoadOrCreateSSHKey(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateSSHKey(path)
	if err != nil || string(a.PublicKey().Marshal()) != string(b.PublicKey().Marshal()) {
		t.Fatal("the key must stay the same", err)
	}
	line := PublicKeyLine(a)
	if !strings.HasPrefix(line, "ssh-ed25519 ") || strings.Contains(line, "\n") {
		t.Fatal(line)
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		t.Fatal(err)
	}
}

// ---- parsing

func TestParseStatusCleansAndLimits(t *testing.T) {
	data := strings.Replace(statusJSON, `"altar22"`, `"al\u001btar\n22"`, 1)
	s, err := ParseStatus([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if s.VMs.List[0].Name != "altar22" {
		t.Fatalf("%q", s.VMs.List[0].Name)
	}
	for name, bad := range map[string]string{"version": strings.Replace(statusJSON, `"v":1`, `"v":2`, 1), "garbage": "<html>", "empty": "", "too big": strings.Repeat(" ", MaxAnswer+1) + statusJSON} {
		if _, err := ParseStatus([]byte(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := ParseStatus([]byte(`{"error":"no data: OSError"}`)); err == nil || !strings.Contains(err.Error(), "no data") {
		t.Fatalf("%v", err)
	}
	// numbers of the wrong kind are rejected, not turned into 0
	if _, err := ParseStatus([]byte(strings.Replace(statusJSON, `"cores":256`, `"cores":"many"`, 1))); err == nil {
		t.Fatal("a string for a number")
	}
}

// What a host with LVM and no ZFS says (agent 0.2.0): the volume group, a nearly full thin pool, the mounted filesystems.
const lvmStatusJSON = `{"v":1,"host":"kvmsfc5","t":1760000000,"interval":60,"cpu":{"cores":128,"idle_pct":70.6,"iowait_pct":0.1,"load":[33.9,30.8,30.8]},
"mem":{"total":1068804202496,"available":386284560384,"used":682519642112,"swap_used":0},
"storage":{"pools":[],"zfs":false,"t":1760000000,
"lvm":[{"name":"am","size":22888000000000,"free":4806000000000,"pvs":1,"lvs":133,"fill":[{"name":"thin","kind":"thin-pool","size":1000000000000,"data_pct":91.5,"meta_pct":null}]}],
"fs":[{"mount":"/","dev":"/dev/md126","type":"ext4","size":49000000000,"used":6000000000,"avail":40000000000},{"mount":"/home/solusvm","dev":"/dev/mapper/am-solusvm","type":"ext4","size":126000000000,"used":69000000000,"avail":47000000000}]},
"vms":{"count":0,"list":[]},"agent":{"version":"0.2.0","python":"3.6.8"},"errors":[]}`

func TestParseStatusWithLVMAndFilesystems(t *testing.T) {
	s, err := ParseStatus([]byte(lvmStatusJSON))
	if err != nil {
		t.Fatal(err)
	}
	st := s.Storage
	if st == nil || len(st.Pools) != 0 || len(st.LVM) != 1 || len(st.FS) != 2 {
		t.Fatalf("%+v", st)
	}
	vg := st.LVM[0]
	if vg.Name != "am" || vg.Size != 22888000000000 || vg.Free != 4806000000000 || vg.PVs != 1 || vg.LVs != 133 || len(vg.Fill) != 1 {
		t.Fatalf("%+v", vg)
	}
	if f := vg.Fill[0]; f.Kind != "thin-pool" || f.DataPct == nil || *f.DataPct != 91.5 || f.MetaPct != nil {
		t.Errorf("a percentage that the host did not say must stay unknown: %+v", f)
	}
	if st.FS[1].Mount != "/home/solusvm" || st.FS[1].Avail != 47000000000 {
		t.Errorf("%+v", st.FS)
	}
	// an agent from before LVM (0.1.0) has neither: that is fine
	old, err := ParseStatus([]byte(statusJSON))
	if err != nil || len(old.Storage.LVM) != 0 || len(old.Storage.FS) != 0 {
		t.Fatalf("%v %+v", err, old)
	}
}

func TestParseStatusCleansAndLimitsTheStorage(t *testing.T) {
	var groups, fs []string
	for i := 0; i < 40; i++ {
		groups = append(groups, `{"name":"vg`+strings.Repeat("x", 150)+`","size":1,"free":1,"pvs":1,"lvs":1}`)
		fs = append(fs, `{"mount":"/m\u001b[31m`+strings.Repeat("y", 150)+`","dev":"d","type":"ext4","size":1,"used":1,"avail":0}`)
	}
	data := `{"v":1,"host":"h","t":1,"storage":{"lvm":[` + strings.Join(groups, ",") + `],"fs":[` + strings.Join(fs, ",") + `]},"vms":{"count":0,"list":[]}}`
	s, err := ParseStatus([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Storage.LVM) != maxVGs || len(s.Storage.FS) != maxFilesystems {
		t.Fatalf("not limited: %d %d", len(s.Storage.LVM), len(s.Storage.FS))
	}
	for _, g := range s.Storage.LVM {
		if len([]rune(g.Name)) > 100 {
			t.Fatalf("name not cut: %d", len(g.Name))
		}
	}
	for _, f := range s.Storage.FS {
		if strings.ContainsRune(f.Mount, 0x1b) || len([]rune(f.Mount)) > 100 {
			t.Fatalf("mount not cleaned: %q", f.Mount)
		}
	}
	// a number of the wrong kind is rejected here too
	if _, err := ParseStatus([]byte(strings.Replace(lvmStatusJSON, `"free":4806000000000`, `"free":"much"`, 1))); err == nil {
		t.Fatal("a string for a number")
	}
}

func TestParseTop(t *testing.T) {
	top, err := ParseTop([]byte(topJSON), "6h")
	if err != nil || len(top.VMs) != 1 || top.VMs[0].PeakCores != 8.1 {
		t.Fatalf("%+v %v", top, err)
	}
	if _, err := ParseTop([]byte(topJSON), "1h"); err == nil {
		t.Fatal("an answer for another window")
	}
}

// ---- poller

type fakeRunner struct {
	mu      sync.Mutex
	answers map[string]string
	errs    map[int64]error
	calls   []string
}

func (r *fakeRunner) Run(ctx context.Context, h store.Host, command string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, fmt.Sprintf("%d:%s", h.ID, command))
	if err := r.errs[h.ID]; err != nil {
		return nil, err
	}
	return []byte(r.answers[command]), nil
}

type fakeSource []store.Host

func (s fakeSource) Hosts(context.Context) ([]store.Host, error) { return s, nil }

func TestPollerKeepsTheLastGoodReadingAndReportsTheError(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{"status": statusJSON}, errs: map[int64]error{}}
	hosts := fakeSource{{ID: 1, Name: "a", Enabled: true}, {ID: 2, Name: "b", Enabled: true}, {ID: 3, Name: "off", Enabled: false}}
	p := NewPoller(hosts, runner)
	p.PollAll(context.Background())
	states := p.States()
	if len(states) != 3 || states[0].Status == nil || states[0].Error != "" || states[2].Error != "disabled" {
		t.Fatalf("%+v", states)
	}
	runner.mu.Lock()
	runner.errs[1] = errors.New("no route to host")
	runner.mu.Unlock()
	p.PollAll(context.Background())
	s, _ := p.State(1)
	if s.Status == nil || s.Error != "no route to host" {
		t.Fatalf("the last good reading must stay with the error: %+v", s)
	}
	hosts = fakeSource{{ID: 2, Name: "b", Enabled: true}}
	p.Source = hosts
	p.PollAll(context.Background())
	if _, ok := p.State(1); ok {
		t.Fatal("a deleted host must disappear")
	}
}

func TestTopIsCachedForAShortTime(t *testing.T) {
	runner := &fakeRunner{answers: map[string]string{"top 6h": topJSON}, errs: map[int64]error{}}
	now := time.Unix(1_760_000_000, 0)
	p := NewPoller(fakeSource{}, runner)
	p.Now = func() time.Time { return now }
	h := store.Host{ID: 7, Name: "a", Enabled: true}
	for i := 0; i < 5; i++ {
		if _, err := p.Top(context.Background(), h, "6h"); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("%d calls", len(runner.calls))
	}
	now = now.Add(time.Minute)
	if _, err := p.Top(context.Background(), h, "6h"); err != nil || len(runner.calls) != 2 {
		t.Fatalf("%v %d", err, len(runner.calls))
	}
	if _, err := p.Top(context.Background(), h, "6h; id"); err == nil {
		t.Fatal("only the known windows")
	}
	if len(runner.calls) != 2 {
		t.Fatal("an unknown window must not reach the host")
	}
}
