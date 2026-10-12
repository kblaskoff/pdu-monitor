package collector

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Known holds the SSH keys of the hosts that a person has confirmed (the fingerprint was compared with the host). The key is only valid
// for the address it was confirmed for.
type Known map[string]string

func knownPath(dir string) string { return filepath.Join(dir, "known_hosts") }

func hostID(name, address string, port int) string {
	return fmt.Sprintf("%s %s:%d", name, address, port)
}

// LoadKnown reads the file; a missing file is an empty list. Lines: `name address:port key-type key`.
func LoadKnown(dir string) (Known, error) {
	k := Known{}
	f, err := os.Open(knownPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return k, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			k[parts[0]+" "+parts[1]] = parts[2]
		}
	}
	return k, sc.Err()
}

// Key is the confirmed key of a host at an address ("" if none).
func (k Known) Key(name, address string, port int) string { return k[hostID(name, address, port)] }

// Confirm remembers a key (authorized_keys format) for a host at an address and writes the file.
func (k Known) Confirm(dir, name, address string, port int, key string) error {
	k[hostID(name, address, port)] = strings.TrimSpace(key)
	ids := make([]string, 0, len(k))
	for id := range k {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("# Keys of the hypervisors that were confirmed with `pdu-collector trust`: name address:port key\n")
	for _, id := range ids {
		b.WriteString(id + " " + k[id] + "\n")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp := knownPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, knownPath(dir))
}
