package collector

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kblaskoff/pdu-monitor/server/internal/hostinfo"
	"github.com/kblaskoff/pdu-monitor/server/internal/store"
)

// Collector reads the hosts and pushes what it read.
type Collector struct {
	Cfg     *Config
	Runner  hostinfo.Runner
	Token   string
	HTTP    *http.Client
	Version string
	Log     func(string)
	Now     func() time.Time

	logMu        sync.Mutex // hosts are read at the same time and all write to the log
	lastTop      time.Time
	lastRejected string
}

// What is sent. It is the same shape the server's /api/collector/report takes: the answer of the agent is passed on as it was.
type hostReport struct {
	Name   string                     `json:"name"`
	Status json.RawMessage            `json:"status,omitempty"`
	Error  string                     `json:"error,omitempty"`
	Tops   map[string]json.RawMessage `json:"tops,omitempty"`
}

type report struct {
	Version string       `json:"version"`
	Hosts   []hostReport `json:"hosts"`
}

type answer struct {
	OK       bool `json:"ok"`
	Accepted int  `json:"accepted"`
	Rejected []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"rejected"`
	Error string `json:"error"`
}

// NewHTTPClient talks TLS 1.2+ and never follows a redirect (the token must not be sent anywhere else).
func NewHTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New(caFile + ": no certificate found in this file")
		}
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, ResponseHeaderTimeout: 20 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// ReadToken reads the token file (one line).
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(data))
	if len(t) < 20 {
		return "", errors.New(path + ": this does not look like a token (make one with pdumonitord collector-token)")
	}
	return t, nil
}

func (c *Collector) logf(format string, a ...any) {
	if c.Log != nil {
		c.logMu.Lock()
		defer c.logMu.Unlock()
		c.Log(fmt.Sprintf(format, a...))
	}
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// storeHost is the host in the form the SSH client wants.
func storeHost(i int, h HostConfig, key string) store.Host {
	return store.Host{ID: int64(i + 1), Name: h.Name, Address: h.Address, Port: h.Port, User: h.User, HostKey: key, Enabled: true}
}

func (c *Collector) readOne(ctx context.Context, i int, h HostConfig, known Known, withTops bool) hostReport {
	out := hostReport{Name: h.Name}
	key := known.Key(h.Name, h.Address, h.Port)
	if key == "" {
		out.Error = "the key of this host is not confirmed: run  pdu-collector trust -host " + h.Name
		return out
	}
	sh := storeHost(i, h, key)
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	raw, err := c.Runner.Run(cctx, sh, "status")
	cancel()
	if err == nil {
		_, err = hostinfo.ParseStatus(raw) // the server checks again; a host that answers rubbish is reported as an error here
		if err == nil && !json.Valid(raw) {
			err = errors.New("the answer is not valid JSON")
		}
	}
	if err != nil {
		out.Error = err.Error()
		c.logf("host %s: %v", h.Name, err)
		return out
	}
	out.Status = json.RawMessage(raw)
	if withTops {
		out.Tops = map[string]json.RawMessage{}
		for _, w := range hostinfo.Windows {
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			raw, err := c.Runner.Run(cctx, sh, "top "+w)
			cancel()
			if err == nil {
				_, err = hostinfo.ParseTop(raw, w)
				if err == nil && !json.Valid(raw) {
					err = errors.New("the answer is not valid JSON")
				}
			}
			if err != nil {
				c.logf("host %s, top %s: %v", h.Name, w, err)
				continue
			}
			out.Tops[w] = json.RawMessage(raw)
		}
	}
	return out
}

// readAll reads the hosts, several at the same time.
func (c *Collector) readAll(ctx context.Context, known Known, withTops bool) []hostReport {
	out := make([]hostReport, len(c.Cfg.Hosts))
	sem := make(chan struct{}, c.Cfg.Parallel)
	var wg sync.WaitGroup
	for i, h := range c.Cfg.Hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = c.readOne(ctx, i, h, known, withTops)
		}()
	}
	wg.Wait()
	return out
}

// Cycle reads every host once and pushes the readings. Every top_seconds the busiest VMs are read as well and pushed separately, so
// that the (slower) reading of them never delays the status. Nothing is kept when the server cannot be reached: the next cycle sends
// fresh readings, and the agents keep their own history.
func (c *Collector) Cycle(ctx context.Context) error {
	known, err := LoadKnown(c.Cfg.DataDir) // read every time: `trust` takes effect without a restart
	if err != nil {
		return err
	}
	if err := c.push(ctx, report{Version: c.Version, Hosts: c.readAll(ctx, known, false)}); err != nil {
		return err
	}
	if c.now().Sub(c.lastTop) < time.Duration(c.Cfg.TopSeconds)*time.Second {
		return nil
	}
	rep := report{Version: c.Version}
	for _, h := range c.readAll(ctx, known, true) {
		if len(h.Tops) > 0 {
			rep.Hosts = append(rep.Hosts, hostReport{Name: h.Name, Tops: h.Tops})
		}
	}
	c.lastTop = c.now()
	if len(rep.Hosts) == 0 {
		return nil
	}
	return c.push(ctx, rep)
}

func (c *Collector) push(ctx context.Context, rep report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Cfg.ServerURL, "/")+"/api/collector/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pdu-collector/"+c.Version)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("the server cannot be reached: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var ans answer
	_ = json.Unmarshal(data, &ans)
	if resp.StatusCode != http.StatusOK {
		msg := ans.Error
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("the server refused the readings: %s", msg)
	}
	var rej []string
	for _, r := range ans.Rejected {
		rej = append(rej, r.Name+" ("+r.Reason+")")
	}
	if text := strings.Join(rej, ", "); text != c.lastRejected {
		c.lastRejected = text
		if text != "" {
			c.logf("the server did not take: %s", text)
		}
	}
	return nil
}

// Run reads and pushes until the context ends.
func (c *Collector) Run(ctx context.Context) {
	lastErr := ""
	for {
		start := time.Now()
		if err := c.Cycle(ctx); err != nil {
			if err.Error() != lastErr { // a server that is down for an hour must not fill the log
				c.logf("%v", err)
			}
			lastErr = err.Error()
		} else {
			if lastErr != "" {
				c.logf("working again")
			}
			lastErr = ""
		}
		wait := time.Duration(c.Cfg.PollSeconds)*time.Second - time.Since(start)
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
