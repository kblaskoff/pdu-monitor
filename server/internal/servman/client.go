// Package servman reads from servman (the inventory of the data centre) which server is plugged into which outlet of which PDU, and plans
// the names of the outlets on the PDUs from it: the outlet is named after the rack unit id (the ccid) of the server, e.g. 200U13.
package servman

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Port is one cable: a PDU (by the name servman gives it, e.g. P200A) and the number of the outlet on it.
type Port struct {
	PDU  string
	Port int
}

// Device is a server (or other device) of servman and the PDU outlets it is plugged into.
type Device struct {
	ID     int64
	CCID   string // the rack unit id, e.g. 200U13: what the outlet is named after
	Name   string // the host name, e.g. a42sf
	Status string
	Ports  []Port
}

// Source gives the devices (the real client, or a fake one in the tests and the demo).
type Source interface {
	Devices(ctx context.Context) ([]Device, error)
}

// Config of the connection. The token is secret: it is read from a file, never kept in the configuration or the repository.
type Config struct {
	URL                string
	Token              string
	CAFile             string
	InsecureSkipVerify bool     // for an internal certificate that nobody has the CA of; prefer CAFile
	Sources            []string // "servers" (the default) and optionally "netswitches"
	Timeout            time.Duration
}

// Client talks to the API with a bearer token. It only reads: it never calls the endpoints that switch power or change records.
type Client struct {
	cfg  Config
	http *http.Client
}

var paths = map[string]string{"servers": "/api/servers/get", "netswitches": "/api/netswitches/get"}

const maxAnswer = 32 << 20

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("servman.url: give the address of the API, e.g. https://servman.example.com")
	}
	if u.Scheme == "http" {
		return nil, errors.New("servman.url: the token must not travel without TLS: use https")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("servman: the token is empty")
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = []string{"servers"}
	}
	for _, s := range cfg.Sources {
		if _, ok := paths[s]; !ok {
			return nil, fmt.Errorf("servman.sources: %q is not known (servers, netswitches)", s)
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New(cfg.CAFile + ": no certificate found in this file")
		}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Client{cfg: cfg, http: &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, InsecureSkipVerify: cfg.InsecureSkipVerify}, //nolint:gosec // an explicit, documented option
			ResponseHeaderTimeout: cfg.Timeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // the token must not follow a redirect
	}}, nil
}

// Devices reads all the devices of the configured sources.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var all []Device
	for _, src := range c.cfg.Sources {
		body, err := c.get(ctx, paths[src]+"?per_page=all")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		devs, err := ParseDevices(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src, err)
		}
		all = append(all, devs...)
	}
	return all, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("cannot be reached: " + strings.ReplaceAll(err.Error(), c.cfg.Token, "***"))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAnswer {
		return nil, errors.New("the answer is too big")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered %s", resp.Status)
	}
	return body, nil
}

// ParseDevices reads the answer of /api/servers/get (and of the same shape from other lists). The answer is {"status":true,"data":...}
// where data is a map of id -> record (or a list). A record has "ccid", "name", "status" and "pdu": a list (or a JSON text of a list) of
// {"pdu_ccid":"P200A","pdu_name":"P200A","pdu_port":"14"}.
func ParseDevices(body []byte) ([]Device, error) {
	var env struct {
		Status *bool           `json:"status"`
		Msg    string          `json:"msg"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, errors.New("the answer is not JSON")
	}
	if env.Status != nil && !*env.Status {
		return nil, fmt.Errorf("servman says: %s", clean(env.Msg, 100))
	}
	records, err := records(env.Data)
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, raw := range records {
		var r struct {
			ID     json.Number     `json:"id"`
			CCID   string          `json:"ccid"`
			Name   string          `json:"name"`
			Status string          `json:"status"`
			PDU    json.RawMessage `json:"pdu"`
		}
		if json.Unmarshal(raw, &r) != nil {
			continue // a record of another shape is not a reason to lose the others
		}
		d := Device{CCID: strings.TrimSpace(r.CCID), Name: strings.TrimSpace(r.Name), Status: strings.TrimSpace(r.Status)}
		d.ID, _ = r.ID.Int64()
		if d.CCID == "" {
			continue
		}
		d.Ports = parsePorts(r.PDU)
		out = append(out, d)
	}
	return out, nil
}

// records unwraps data: an object of records, a list, or one more level of {"data": ...}.
func records(data json.RawMessage) ([]json.RawMessage, error) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil, errors.New("the answer has no data")
	}
	if data[0] == '[' {
		var list []json.RawMessage
		return list, json.Unmarshal(data, &list)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, errors.New("the data is neither a list nor an object")
	}
	if inner, ok := obj["data"]; ok && len(obj) <= 4 {
		return records(inner)
	}
	out := make([]json.RawMessage, 0, len(obj))
	for _, v := range obj {
		out = append(out, v)
	}
	return out, nil
}

func parsePorts(raw json.RawMessage) []Port {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' { // a JSON text of the list
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil
		}
		raw = []byte(text)
	}
	var list []struct {
		CCID string          `json:"pdu_ccid"`
		Name string          `json:"pdu_name"`
		Port json.RawMessage `json:"pdu_port"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var ports []Port
	for _, p := range list {
		pdu := strings.TrimSpace(p.CCID)
		if pdu == "" {
			pdu = strings.TrimSpace(p.Name)
		}
		n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(string(p.Port)), `"`))
		if pdu == "" || err != nil || n < 1 {
			continue
		}
		ports = append(ports, Port{PDU: pdu, Port: n})
	}
	return ports
}

func clean(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= ' ' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > max {
		out = string(r[:max])
	}
	return out
}
