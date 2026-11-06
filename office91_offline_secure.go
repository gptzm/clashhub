package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed office91_profile_crypto.py
var office91TrustedCrypto string

const office91TrustedBootstrapSHA256 = "002db25b2b402379420b9ea490c870cf7b13ade220ec18f21deec84291a61b57"

func office91PythonCommand(ctx context.Context, pythonPath string, args ...string) (*exec.Cmd, error) {
	if !filepath.IsAbs(pythonPath) {
		return nil, errors.New("offline-python-requires-absolute-path")
	}
	cmd := exec.CommandContext(ctx, pythonPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd, nil
}

// Production entry point: the crypto parser is embedded, never loaded from an
// arbitrary path. The key path contains only local protected key material.
func importOffice91OfflineSecure(ctx context.Context, profilePath, keyPath, pythonPath, validatorPath string) error {
	if !filepath.IsAbs(keyPath) {
		return errors.New("offline-key-requires-absolute-path")
	}
	return importOffice91OfflineModule(ctx, profilePath, pythonPath, validatorPath, office91TrustedCrypto, keyPath)
}

// Optional one-time local bootstrap from the user's previously verified tool.
// The exact bytes are hash-checked then executed from memory; no mutable tool
// pathname is executed after verification. No credentials reach stdout/stderr.
// Existing keys are never overwritten. Callers must explicitly authorize this
// local initialization; it does not run merely by importing the module.
func initializeOffice91OfflineKey(ctx context.Context, keyPath, trustedScriptPath, pythonPath string) error {
	if !filepath.IsAbs(keyPath) || !filepath.IsAbs(trustedScriptPath) {
		return errors.New("offline-key-requires-absolute-path")
	}
	b, err := os.ReadFile(trustedScriptPath)
	if err != nil {
		return errors.New("offline-key-bootstrap-unavailable")
	}
	digest := sha256.Sum256(b)
	if hex.EncodeToString(digest[:]) != office91TrustedBootstrapSHA256 {
		return errors.New("offline-key-bootstrap-identity-mismatch")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("offline-bootstrap-randomness-unavailable")
	}
	temporary := ".office91-key-" + hex.EncodeToString(nonce)
	defer os.Remove(filepath.Join(filepath.Dir(keyPath), temporary))
	cmd, err := office91PythonCommand(ctx, pythonPath, "-I", "-c", office91BootstrapKey, keyPath, temporary)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(string(b))
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("offline-key-bootstrap-failed")
	}
	return nil
}

const office91BootstrapKey = `
import os, pathlib, stat, sys, types
try:
    module=types.ModuleType("frozen_office91_bootstrap")
    exec(compile(sys.stdin.read(),"<frozen-office91-bootstrap>","exec"),module.__dict__)
    key=module.derive()
    if not isinstance(key,bytes) or len(key)!=32: raise ValueError()
    path=pathlib.Path(sys.argv[1]);parent=path.parent
    parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    directory=os.open(parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:
        st=os.fstat(directory)
        if st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)&0o077: raise ValueError()
        temporary=sys.argv[2]
        try:
            fd=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=directory)
            with os.fdopen(fd,"wb") as out:
                out.write(key);out.flush();os.fsync(out.fileno())
            # Link provides an atomic exclusive publication, never replacement.
            os.link(temporary,path.name,src_dir_fd=directory,dst_dir_fd=directory,follow_symlinks=False)
            os.fsync(directory)
        finally:
            try: os.unlink(temporary,dir_fd=directory)
            except FileNotFoundError: pass
    finally: os.close(directory)
except BaseException:
    sys.exit(1)
`
