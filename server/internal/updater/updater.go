// Package updater is the application's half of the "Update" button. The application never updates itself and never runs a command: it
// writes a file (update-request) into its own data directory. A systemd path unit that belongs to root sees the file and starts
// deploy/update.sh, which fetches the repository, builds, installs, restarts and goes back to the old binary when the new one does not
// stay up. The script leaves its state in update.status and its output in update.log, which is what the application shows.
package updater

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	RequestFile = "update-request" // written by the application, removed by the script when it starts
	MarkerFile  = ".updater"       // written by the install script: the root side (path unit and script) is there
	StatusFile  = "update.status"  // "state<TAB>unix time<TAB>detail"; state is running, ok, uptodate or failed
	LogFile     = "update.log"
)

// ErrNotInstalled is returned when the root side was never installed on this machine.
var ErrNotInstalled = errors.New("the updater is not installed here: run the install script of this part once more (see the README)")

// ErrPending is returned when a request has been made and the script has not picked it up yet.
var ErrPending = errors.New("an update has been requested already")

// Installed reports whether the install script put the updater in place.
func Installed(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, MarkerFile))
	return err == nil
}

// Request asks the root side to update. who is written into the file for the log only; nothing in the file is ever used as a command.
func Request(dir, who string) error {
	if !Installed(dir) {
		return ErrNotInstalled
	}
	path := filepath.Join(dir, RequestFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		return ErrPending
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%d\n", strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, who), time.Now().Unix())
	return err
}

// Status is what the screen shows of the last update.
type Status struct {
	Available bool       `json:"available"`       // the root side is installed
	Pending   bool       `json:"pending"`         // requested, not started yet
	State     string     `json:"state,omitempty"` // running, ok, uptodate, failed (empty: never updated from here)
	At        *time.Time `json:"at,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	Log       []string   `json:"log,omitempty"` // the last lines of the output of the script
}

const maxLogLines = 40

// Read looks at the files. It never fails: a missing file is "no update yet".
func Read(dir string) Status {
	st := Status{Available: Installed(dir)}
	if _, err := os.Stat(filepath.Join(dir, RequestFile)); err == nil {
		st.Pending = true
	}
	if data, err := os.ReadFile(filepath.Join(dir, StatusFile)); err == nil {
		parts := strings.SplitN(strings.TrimSpace(string(data)), "\t", 3)
		switch parts[0] {
		case "running", "ok", "uptodate", "failed":
			st.State = parts[0]
			if len(parts) > 1 {
				if sec, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
					at := time.Unix(sec, 0)
					st.At = &at
				}
			}
			if len(parts) > 2 {
				st.Detail = clean(parts[2], 300)
			}
		}
	}
	if f, err := os.Open(filepath.Join(dir, LogFile)); err == nil {
		defer f.Close()
		var lines []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 64*1024)
		for sc.Scan() {
			lines = append(lines, clean(sc.Text(), 300))
			if len(lines) > maxLogLines {
				lines = lines[1:]
			}
		}
		st.Log = lines
	}
	return st
}

// clean makes text from a file safe to show: printable characters only, and not too long.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
