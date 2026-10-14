package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestNeedsTheInstalledUpdater(t *testing.T) {
	dir := t.TempDir()
	if err := Request(dir, "anna"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("%v", err)
	}
	if Read(dir).Available {
		t.Error("available without the marker")
	}
	_ = os.WriteFile(filepath.Join(dir, MarkerFile), nil, 0o644)
	if err := Request(dir, "an\nna\x1b"); err != nil {
		t.Fatal(err)
	}
	if err := Request(dir, "anna"); !errors.Is(err, ErrPending) {
		t.Errorf("second: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, RequestFile))
	if strings.Count(string(data), "\n") != 1 || strings.ContainsAny(string(data), "\x1b") || !strings.HasPrefix(string(data), "anna\t") {
		t.Errorf("the file is not one clean line: %q", data)
	}
	if st := Read(dir); !st.Pending || !st.Available || st.State != "" {
		t.Errorf("%+v", st)
	}
}

func TestReadStatusAndLog(t *testing.T) {
	dir := t.TempDir()
	if st := Read(dir); st.State != "" || st.Pending || len(st.Log) != 0 {
		t.Errorf("empty: %+v", st)
	}
	_ = os.WriteFile(filepath.Join(dir, StatusFile), []byte("ok\t1700000000\told -> new\n"), 0o644)
	var log strings.Builder
	for i := 0; i < 100; i++ {
		log.WriteString("line\n")
	}
	log.WriteString("last\n")
	_ = os.WriteFile(filepath.Join(dir, LogFile), []byte(log.String()), 0o644)
	st := Read(dir)
	if st.State != "ok" || st.Detail != "old -> new" || st.At == nil || st.At.Unix() != 1700000000 {
		t.Errorf("%+v", st)
	}
	if len(st.Log) != maxLogLines || st.Log[len(st.Log)-1] != "last" {
		t.Errorf("the log is cut to the end: %d %q", len(st.Log), st.Log[len(st.Log)-1])
	}
	_ = os.WriteFile(filepath.Join(dir, StatusFile), []byte("bogus\t1\tx\n"), 0o644)
	if Read(dir).State != "" {
		t.Error("an unknown state is shown")
	}
}
