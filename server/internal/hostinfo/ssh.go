package hostinfo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// ErrNotTrusted: the key of the host has not been confirmed by an administrator yet.
var ErrNotTrusted = errors.New("the key of this host is not confirmed yet: open the host and confirm its fingerprint")

// Runner runs one of the fixed hostinfo commands on a host and returns what it printed.
type Runner interface {
	Run(ctx context.Context, h store.Host, command string) ([]byte, error)
}

// SSHClient is the real Runner. It keeps one connection per host and reconnects when it breaks. The key of the host must be the one
// an administrator confirmed (pinned): there is no "trust on first use" that nobody sees.
type SSHClient struct {
	signer  ssh.Signer
	Timeout time.Duration
	mu      sync.Mutex
	conns   map[int64]*pooled
}

type pooled struct {
	client *ssh.Client
	addr   string
	key    string
	user   string
}

// LoadOrCreateSSHKey reads the private key of this server (OpenSSH format) or creates a new ed25519 one.
func LoadOrCreateSSHKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, "pdu-monitor")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			return nil, err
		}
		return ssh.NewSignerFromKey(priv)
	}
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

// PublicKeyLine is what goes into the install script of the agent (--key).
func PublicKeyLine(s ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " pdu-monitor"
}

func NewSSHClient(signer ssh.Signer) *SSHClient {
	return &SSHClient{signer: signer, Timeout: 10 * time.Second, conns: map[int64]*pooled{}}
}

func hostKeyAlgorithms(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{k.Type()}
}

func (c *SSHClient) config(user string, callback ssh.HostKeyCallback, algs []string) *ssh.ClientConfig {
	return &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(c.signer)}, HostKeyCallback: callback, HostKeyAlgorithms: algs, Timeout: c.Timeout}
}

func address(h store.Host) string { return net.JoinHostPort(h.Address, strconv.Itoa(h.Port)) }

// Run runs a command with a connection to the host.
func (c *SSHClient) Run(ctx context.Context, h store.Host, command string) ([]byte, error) {
	if h.HostKey == "" {
		return nil, ErrNotTrusted
	}
	trusted, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.HostKey))
	if err != nil {
		return nil, fmt.Errorf("the stored key of the host is damaged: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		client, fresh, err := c.connection(ctx, h, trusted)
		if err != nil {
			return nil, err
		}
		out, err := c.exec(ctx, client, command)
		if err == nil {
			return out, nil
		}
		c.drop(h.ID, client)
		if fresh || ctx.Err() != nil {
			return nil, err // a connection that was just made failed: no second try
		}
	}
	return nil, errors.New("could not run the command")
}

func (c *SSHClient) connection(ctx context.Context, h store.Host, trusted ssh.PublicKey) (*ssh.Client, bool, error) {
	key := string(ssh.MarshalAuthorizedKey(trusted))
	c.mu.Lock()
	if p := c.conns[h.ID]; p != nil && p.addr == address(h) && p.key == key && p.user == h.User {
		c.mu.Unlock()
		return p.client, false, nil
	} else if p != nil {
		p.client.Close()
		delete(c.conns, h.ID)
	}
	c.mu.Unlock()

	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	raw, err := d.DialContext(dialCtx, "tcp", address(h))
	if err != nil {
		return nil, true, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, address(h), c.config(h.User, ssh.FixedHostKey(trusted), hostKeyAlgorithms(trusted)))
	if err != nil {
		raw.Close()
		return nil, true, friendlySSH(err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	c.mu.Lock()
	if old := c.conns[h.ID]; old != nil {
		old.client.Close()
	}
	c.conns[h.ID] = &pooled{client: client, addr: address(h), key: key, user: h.User}
	c.mu.Unlock()
	return client, true, nil
}

func (c *SSHClient) drop(id int64, client *ssh.Client) {
	c.mu.Lock()
	if p := c.conns[id]; p != nil && p.client == client {
		delete(c.conns, id)
	}
	c.mu.Unlock()
	client.Close()
}

// Close closes all connections.
func (c *SSHClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, p := range c.conns {
		p.client.Close()
		delete(c.conns, id)
	}
}

func (c *SSHClient) exec(ctx context.Context, client *ssh.Client, command string) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(stdout, MaxAnswer+1))
		if err == nil && len(data) > MaxAnswer {
			err = errors.New("the answer is too big")
		}
		if err == nil {
			// a non-zero exit status still has a JSON answer with the reason: it is parsed by the caller
			if werr := session.Wait(); werr != nil {
				var exit *ssh.ExitError
				if !errors.As(werr, &exit) {
					err = werr
				}
			}
		}
		done <- result{data, err}
	}()
	timeout := time.NewTimer(c.Timeout * 2)
	defer timeout.Stop()
	select {
	case r := <-done:
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return nil, errors.New("the host did not answer in time")
	}
}

// FetchHostKey connects and returns the key the host offers, without logging in. The administrator compares its fingerprint with the one
// shown on the host (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub) and then confirms it.
func (c *SSHClient) FetchHostKey(ctx context.Context, addr string, port int) (ssh.PublicKey, error) {
	var d net.Dialer
	dialCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	target := net.JoinHostPort(addr, strconv.Itoa(port))
	raw, err := d.DialContext(dialCtx, "tcp", target)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(c.Timeout))
	var got ssh.PublicKey
	stop := errors.New("key received")
	cfg := &ssh.ClientConfig{User: "probe", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error { got = key; return stop }, Timeout: c.Timeout}
	_, _, _, err = ssh.NewClientConn(raw, target, cfg)
	if got == nil {
		if err == nil {
			err = errors.New("no host key received")
		}
		return nil, err
	}
	return got, nil
}

func friendlySSH(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "host key") || strings.Contains(err.Error(), "knownhosts"):
		return fmt.Errorf("the host presents a different key than the confirmed one (reinstalled, or someone is in the way): %w", err)
	case strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "no supported methods"):
		return errors.New("the host refused the key of this server (install the agent with this server's public key)")
	}
	return err
}
