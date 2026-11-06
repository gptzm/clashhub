package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOffice91OfflineIdentity(t *testing.T) {
	d := t.TempDir()
	if _, err := discoverOffice91OfflineProfile(d); err == nil {
		t.Fatal("missing accepted")
	}
	for _, n := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("encrypted"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := discoverOffice91OfflineProfile(d); err == nil {
		t.Fatal("ambiguous identity guessed")
	}
	os.Remove(filepath.Join(d, "b.yaml"))
	if _, err := discoverOffice91OfflineProfile(d); err != nil {
		t.Fatal(err)
	}
}
func TestOffice91OfflineAuthenticatedAtomicImport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := t.TempDir()
	module := filepath.Join(d, "extractor.py")
	// Synthetic test key only; no real profile, plaintext or credentials are read.
	code := `import yaml
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
UniqueLoader=yaml.SafeLoader
KNOWN={"ss","invalid-protocol"}
def derive(): return bytes(range(32))
`
	if err := os.WriteFile(module, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(d, "input.yaml")
	encrypt := func(protocol string) {
		t.Helper()
		script := `import base64,pathlib,sys
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
plain=("proxies:\n- name: synthetic\n  type: "+sys.argv[2]+"\n  server: example.test\n  port: 443\n  cipher: aes-128-gcm\n  password: synthetic-test\n  udp: true\n").encode()
nonce=bytes(range(12));pathlib.Path(sys.argv[1]).write_bytes(base64.b64encode(nonce+AESGCM(bytes(range(32))).encrypt(nonce,plain,None)))`
		if err := exec.Command("python3", "-I", "-c", script, profile, protocol).Run(); err != nil {
			t.Skip("Python crypto dependencies unavailable")
		}
	}
	validator := "/opt/homebrew/bin/mihomo"
	if _, err := os.Stat(validator); err != nil {
		t.Skip("Mihomo unavailable")
	}
	encrypt("ss")
	if err := importOffice91Offline(context.Background(), profile, module, validator); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(appSupportDir(), "sources", "office91.yaml")
	old, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(old, []byte("udp: true")) || !bytes.Contains(old, []byte("cipher: aes-128-gcm")) {
		t.Fatal("node parameters dropped")
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0600 {
		t.Fatal("cache not private")
	}
	checkOld := func() {
		t.Helper()
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, old) {
			t.Fatal("last good cache replaced")
		}
	}
	if err := os.WriteFile(profile, []byte("unauthenticated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := importOffice91Offline(context.Background(), profile, module, validator); err == nil {
		t.Fatal("authentication failure accepted")
	}
	checkOld()
	encrypt("invalid-protocol") // YAML valid but Mihomo must reject it.
	if err := importOffice91Offline(context.Background(), profile, module, validator); err == nil {
		t.Fatal("Mihomo rejection accepted")
	}
	checkOld()
	encrypt("ss")
	slow := filepath.Join(d, "slow-validator")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := importOffice91Offline(canceled, profile, module, slow); err == nil {
		t.Fatal("canceled validation accepted")
	}
	checkOld()
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	attacks := map[string]string{
		"candidate-content": `import os,sys
fd=os.open(sys.argv[-1],os.O_RDWR);os.pwrite(fd,b"corrupted",0);os.close(fd)
`,
		"candidate-identity": `import os
name=next(n for n in os.listdir(".") if n.startswith(".office91-offline-"))
os.unlink(name)
fd=os.open(name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.write(fd,b"proxies: []");os.close(fd)
`,
		"parent-identity": `import os
parent=os.getcwd();os.rename(parent,parent+".swapped");os.mkdir(parent,0o700)
`,
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			validator := filepath.Join(d, "attack-"+name)
			if err := os.WriteFile(validator, []byte("#!"+pythonPath+"\n"+attack), 0700); err != nil {
				t.Fatal(err)
			}
			if err := importOffice91Offline(context.Background(), profile, module, validator); err == nil {
				t.Fatal("identity/content replacement published")
			}
			parent := filepath.Dir(target)
			if name == "parent-identity" {
				// The old directory is held by fd throughout validation; restore
				// its name only inside this isolated fixture before checking it.
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(parent+".swapped", parent); err != nil {
					t.Fatal(err)
				}
			}
			checkOld()
		})
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".office91-offline-*"))
	if len(files) != 0 {
		t.Fatal("sensitive temporary files remain")
	}
}

func TestOffice91OfflineRealProfileIsolated(t *testing.T) {
	if os.Getenv("CLASHHUB_OFFLINE_LIVE") != "1" {
		t.Skip("explicit live fixture validation only")
	}
	realHome := userHome()
	source, err := discoverOffice91OfflineProfile(filepath.Join(realHome, "Library", "Application Support", "com.abysswalker.office91", "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	extractor := filepath.Join(realHome, "Library", "Application Support", "ClashHub", "reverse", "work", "verify", "offline_profile_extract.py")
	t.Setenv("HOME", t.TempDir())
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python unavailable")
	}
	keyPath := filepath.Join(appSupportDir(), "secrets", "office91.key")
	if err := initializeOffice91OfflineKey(context.Background(), keyPath, extractor, pythonPath); err != nil {
		t.Fatal("trusted bootstrap failed")
	}
	if err := initializeOffice91OfflineKey(context.Background(), keyPath, extractor, pythonPath); err == nil {
		t.Fatal("existing private key overwritten")
	}
	if err := importOffice91OfflineSecure(context.Background(), source, keyPath, pythonPath, "/opt/homebrew/bin/mihomo"); err != nil {
		t.Fatal("real profile validation failed")
	}
	target := filepath.Join(appSupportDir(), "sources", "office91.yaml")
	old, err := os.ReadFile(target)
	if err != nil {
		t.Fatal("private cache missing")
	}
	st, err := os.Stat(target)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("cache permissions invalid")
	}
	// Flip an authenticated byte in an isolated ciphertext copy, never the source.
	encoded, err := os.ReadFile(source)
	if err != nil {
		t.Fatal("source unavailable")
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil || len(raw) < 28 {
		t.Fatal("fixture invalid")
	}
	raw[len(raw)-1] ^= 1
	bad := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(bad, []byte(base64.StdEncoding.EncodeToString(raw)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := importOffice91OfflineSecure(context.Background(), bad, keyPath, pythonPath, "/opt/homebrew/bin/mihomo"); err == nil {
		t.Fatal("invalid authentication accepted")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(after, old) {
		t.Fatal("failed authentication replaced cache")
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal("permission test failed")
	}
	if err := importOffice91OfflineSecure(context.Background(), source, keyPath, pythonPath, "/opt/homebrew/bin/mihomo"); err == nil {
		t.Fatal("nonprivate key accepted")
	}
	after, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(after, old) {
		t.Fatal("invalid key permissions replaced cache")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		t.Fatal("permission restore failed")
	}
	// Counts only, with no names, URLs, server addresses or secrets in output.
	countScript := `import pathlib,sys,yaml
x=yaml.safe_load(pathlib.Path(sys.argv[1]).read_text());print(len(x["proxies"]))`
	out, err := exec.Command("python3", "-I", "-c", countScript, target).Output()
	if err != nil {
		t.Fatal("count failed")
	}
	fmt.Printf("authenticated=true mihomo_accepted=true private_cache_mode=0600 previous_cache_retained=true nodes=%s", out)
}

func TestOffice91BootstrapRejectsUntrustedTool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := t.TempDir()
	tool := filepath.Join(d, "untrusted.py")
	if err := os.WriteFile(tool, []byte("raise RuntimeError()"), 0600); err != nil {
		t.Fatal(err)
	}
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python unavailable")
	}
	target := filepath.Join(d, "key")
	if err := initializeOffice91OfflineKey(context.Background(), target, tool, pythonPath); err == nil {
		t.Fatal("untrusted bootstrap accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("untrusted tool wrote key")
	}
}

// Synthetic fixtures may supply a local module; production uses embedded crypto only.
func importOffice91Offline(ctx context.Context, profilePath, extractorPath, validatorPath string) error {
	if !filepath.IsAbs(extractorPath) {
		return fmt.Errorf("offline-extractor-requires-absolute-path")
	}
	module, err := os.ReadFile(extractorPath)
	if err != nil {
		return fmt.Errorf("offline-extractor-unavailable")
	}
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		return fmt.Errorf("offline-python-unavailable")
	}
	return importOffice91OfflineModule(ctx, profilePath, pythonPath, validatorPath, string(module), "")
}
