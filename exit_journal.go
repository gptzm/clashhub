package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Intent is persisted before networksetup and kept after completion. The file is
// independent of state.json, which the daemon may rewrite during shutdown.
type exitIntent struct {
	Before ProxySetting `json:"before"`
	After  ProxySetting `json:"after"`
	Done   bool         `json:"done"`
}
type exitJournal struct {
	Version int                   `json:"version"`
	Session string                `json:"session"`
	Fields  map[string]exitIntent `json:"fields"`
}

var persistExitJournal = writeExitJournal

func exitSessionID(services []ServiceSnapshot, httpListen, socksListen string) (string, error) {
	st, err := readState()
	if err != nil {
		return "", err
	}
	var started string
	if st != nil && st.AppliedHTTP == httpListen && st.AppliedSOCKS == socksListen {
		started = st.StartedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	b, err := json.Marshal(struct {
		Services             []ServiceSnapshot
		HTTP, SOCKS, Started string
	}{services, httpListen, socksListen, started})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
func exitJournalPath(id string) string { return filepath.Join(appSupportDir(), "exit-"+id+".json") }
func loadExitJournal(id string) (*exitJournal, error) {
	j := &exitJournal{Version: 1, Session: id, Fields: map[string]exitIntent{}}
	b, err := os.ReadFile(exitJournalPath(id))
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, j); err != nil {
		return nil, fmt.Errorf("read exit journal: %w", err)
	}
	if j.Version != 1 || j.Session != id || j.Fields == nil {
		return nil, fmt.Errorf("invalid exit journal")
	}
	return j, nil
}
func writeExitJournal(j *exitJournal) error {
	dir := appSupportDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".exit-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, exitJournalPath(j.Session)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
