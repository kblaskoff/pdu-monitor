// Package collector is the passive reader of hypervisors: it asks the agents over SSH and pushes the answers to the server. It has no
// web interface, no login and listens on no port.
package collector

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostConfig is one hypervisor to read.
type HostConfig struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
	User    string `yaml:"user"`
}

// Config is the configuration file of the collector.
type Config struct {
	// ServerURL is where the web server of PDU Monitor is, e.g. https://pdu.example.com:8443 (https is required, except for localhost).
	ServerURL string `yaml:"server_url"`
	// TokenFile holds the token that pdumonitord collector-token made (one line, mode 600).
	TokenFile string `yaml:"token_file"`
	// CAFile is the certificate of your own CA, if the server certificate is not from a public one.
	CAFile string `yaml:"ca_file"`
	// DataDir holds the SSH key of the collector and the confirmed keys of the hosts (known_hosts).
	DataDir string `yaml:"data_dir"`
	// PollSeconds is how often every host is read; TopSeconds how often the busiest VMs of every period are read.
	PollSeconds int          `yaml:"poll_seconds"`
	TopSeconds  int          `yaml:"top_seconds"`
	Parallel    int          `yaml:"parallel"`
	Hosts       []HostConfig `yaml:"hosts"`
}

// Load reads and checks the file. Unknown keys are an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) finish() error {
	u, err := url.Parse(c.ServerURL)
	if err != nil || u.Host == "" {
		return errors.New("server_url: give the address of the server, e.g. https://pdu.example.com:8443")
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("server_url: the token must not travel without TLS: use https (http is only accepted for localhost)")
		}
	default:
		return errors.New("server_url: it must start with https://")
	}
	if c.TokenFile == "" {
		return errors.New("token_file: say where the token is")
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/pdu-collector"
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 60
	}
	if c.PollSeconds < 15 || c.PollSeconds > 3600 {
		return errors.New("poll_seconds: between 15 and 3600 (the agents take a sample a minute)")
	}
	if c.TopSeconds == 0 {
		c.TopSeconds = 300
	}
	if c.TopSeconds < 60 || c.TopSeconds > 86400 {
		return errors.New("top_seconds: between 60 and 86400")
	}
	if c.Parallel <= 0 {
		c.Parallel = 8
	}
	if len(c.Hosts) == 0 {
		return errors.New("hosts: list at least one hypervisor")
	}
	seen := map[string]bool{}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		h.Name, h.Address, h.User = strings.TrimSpace(h.Name), strings.TrimSpace(h.Address), strings.TrimSpace(h.User)
		if h.Name == "" || len(h.Name) > 100 || strings.ContainsAny(h.Name, " \t\r\n") {
			return fmt.Errorf("hosts[%d]: the name must have 1 to 100 characters, without spaces", i)
		}
		if seen[h.Name] {
			return fmt.Errorf("hosts: the name %q is used twice", h.Name)
		}
		seen[h.Name] = true
		if h.Address == "" || strings.ContainsAny(h.Address, " \t/@") {
			return fmt.Errorf("hosts[%d] (%s): enter the address", i, h.Name)
		}
		if h.Port == 0 {
			h.Port = 22
		}
		if h.Port < 1 || h.Port > 65535 {
			return fmt.Errorf("hosts[%d] (%s): bad port", i, h.Name)
		}
		if h.User == "" {
			h.User = "hostinfo-ro"
		}
		if strings.ContainsAny(h.User, " \t@:/") {
			return fmt.Errorf("hosts[%d] (%s): bad user name", i, h.Name)
		}
	}
	return nil
}

// Example is written by `pdu-collector init`.
const Example = `# PDU Monitor collector: reads the hypervisors over SSH and pushes the readings to the server. It has no web interface and
# listens on no port. Keep this file readable only by the service user (chmod 640).

server_url: "https://pdu.example.com:8443"
token_file: /etc/pdu-collector/token      # made on the server with: pdumonitord collector-token -name collector-a
# ca_file: /etc/pdu-collector/ca.pem      # the certificate of your internal CA, if the server's certificate comes from it
data_dir: /var/lib/pdu-collector

poll_seconds: 60
top_seconds: 300

# user is the read-only account the agent installer made (hostinfo-ro). Confirm the key of each host once:
#   sudo -u pdu-collector pdu-collector trust -config /etc/pdu-collector/config.yaml -host a39sf
hosts:
  - name: a39sf
    address: 10.1.0.39
  - name: a40sf
    address: 10.1.0.40
`
