package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// office91OfflineRequest is an explicit source action. Paths are never written
// to config.json or inferred from an arbitrary filename. The selected offline
// mode itself is persisted in a private marker so an online refresher cannot
// silently take ownership of the same office91 cache after restart.
type office91OfflineRequest struct {
	ProfilePath   string
	KeyPath       string
	PythonPath    string
	ValidatorPath string
}

// office91OfflineRuntime schedules authenticated offline imports. It does not
// start an app, probe the fallback port, or make a subscription request. The importer owns
// authentication, atomic cache replacement, and Mihomo validation.
type office91OfflineRuntime struct {
	mu sync.Mutex

	running bool
	// sourceEnabled is retained for isolated callers that construct a runtime
	// directly. Production runtimes use sourceEnabledFn so a GUI source toggle
	// is observed without recreating the subscription API object.
	sourceEnabled   bool
	sourceEnabledFn func() (bool, error)
	status          SubscriptionStatus
	defaultKey      string
	defaultPython   string
	defaultEngine   string
	timeout         time.Duration
	importer        func(context.Context, string, string, string, string) error
	bootstrapPath   string
	bootstrap       func(context.Context, string, string, string) error
	// Production API imports must run through the daemon transaction. A runtime
	// built directly by an isolated caller keeps the old cache-only behavior.
	daemonTransactionRequired bool
}

var (
	office91OfflineUpdateMu    sync.Mutex
	office91OfflineUpdate      func()
	office91OfflineApplyMu     sync.Mutex
	office91OfflineApply       func() error
	office91OfflineRollback    func() error
	office91OfflineTransaction func(context.Context, func(context.Context, func() error, func() error) error) error
	// Every writer of sources/office91.yaml takes this lock. The offline
	// transaction holds it across mode switching, import, apply and rollback;
	// the online refresher checks the mode again while holding the same lock.
	office91SourceWriteMu sync.Mutex
)

const (
	office91ModeOnline  = "online"
	office91ModeOffline = "offline"
)

func office91ModePath() string { return filepath.Join(sourcesCacheDir(), "office91.mode") }

// readOffice91Mode treats a missing marker as the legacy online mode. Any
// present but malformed marker fails closed: an online refresher must not
// write a cache whose ownership is unclear.
func readOffice91Mode() (string, error) {
	b, err := os.ReadFile(office91ModePath())
	if errors.Is(err, os.ErrNotExist) {
		return office91ModeOnline, nil
	}
	if err != nil || len(b) > 32 {
		return "", errors.New("offline-mode-invalid")
	}
	mode := strings.TrimSpace(string(b))
	if mode != office91ModeOnline && mode != office91ModeOffline {
		return "", errors.New("offline-mode-invalid")
	}
	return mode, nil
}

func office91OfflineModeActive() (bool, error) {
	mode, err := readOffice91Mode()
	if err != nil {
		return true, err
	}
	return mode == office91ModeOffline, nil
}

func writeOffice91Mode(mode string) error {
	if mode != office91ModeOnline && mode != office91ModeOffline {
		return errors.New("offline-mode-invalid")
	}
	return writePrivateFile(office91ModePath(), mode+"\n")
}

type office91ModeSnapshot struct {
	exists bool
	mode   string
}

func snapshotOffice91Mode() (office91ModeSnapshot, error) {
	_, err := os.Stat(office91ModePath())
	if errors.Is(err, os.ErrNotExist) {
		return office91ModeSnapshot{}, nil
	}
	if err != nil {
		return office91ModeSnapshot{}, errors.New("offline-mode-unavailable")
	}
	mode, err := readOffice91Mode()
	if err != nil {
		return office91ModeSnapshot{}, err
	}
	return office91ModeSnapshot{exists: true, mode: mode}, nil
}

func restoreOffice91Mode(snapshot office91ModeSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(office91ModePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("offline-mode-rollback-failed")
		}
		return nil
	}
	if err := writeOffice91Mode(snapshot.mode); err != nil {
		return errors.New("offline-mode-rollback-failed")
	}
	return nil
}

// setOffice91OfflineApply is installed only while the App runtime is alive.
// It is intentionally optional: command-line or isolated callers can still
// produce the authenticated private cache without starting an App.
func setOffice91OfflineApply(apply func() error, rollback func() error) {
	office91OfflineApplyMu.Lock()
	office91OfflineApply = apply
	office91OfflineRollback = rollback
	office91OfflineApplyMu.Unlock()
}

func office91OfflineApplyHooks() (func() error, func() error) {
	office91OfflineApplyMu.Lock()
	defer office91OfflineApplyMu.Unlock()
	return office91OfflineApply, office91OfflineRollback
}

// The daemon transaction owns sourceRegenMu for snapshot through rollback.
func setOffice91OfflineTransaction(fn func(context.Context, func(context.Context, func() error, func() error) error) error) {
	office91OfflineApplyMu.Lock()
	defer office91OfflineApplyMu.Unlock()
	office91OfflineTransaction = fn
}
func office91OfflineTransactionHook() func(context.Context, func(context.Context, func() error, func() error) error) error {
	office91OfflineApplyMu.Lock()
	defer office91OfflineApplyMu.Unlock()
	return office91OfflineTransaction
}

// A running daemon installs this optional hook from source_runtime.go. A
// caller without a daemon still gets a usable private cache after success.
func setOffice91OfflineUpdate(fn func()) {
	office91OfflineUpdateMu.Lock()
	office91OfflineUpdate = fn
	office91OfflineUpdateMu.Unlock()
}

func notifyOffice91OfflineUpdate() {
	office91OfflineUpdateMu.Lock()
	fn := office91OfflineUpdate
	office91OfflineUpdateMu.Unlock()
	if fn != nil {
		fn()
	}
}

func defaultOffice91PythonPath() string {
	p, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func newOffice91OfflineRuntime(cfg Config) *office91OfflineRuntime {
	engine := engineBinary(cfg)
	enabled := office91SourceEnabledInConfig(cfg)
	runtime := &office91OfflineRuntime{
		status:                    SubscriptionStatus{SourceID: sourceIDOffice91, Status: "unknown"},
		sourceEnabled:             enabled,
		defaultKey:                filepath.Join(appSupportDir(), "secrets", "office91.key"),
		defaultPython:             defaultOffice91PythonPath(),
		defaultEngine:             engine,
		timeout:                   60 * time.Second,
		importer:                  importOffice91OfflineSecure,
		bootstrapPath:             filepath.Join(userHome(), "Library", "Application Support", "ClashHub", "reverse", "work", "verify", "offline_profile_extract.py"),
		bootstrap:                 initializeOffice91OfflineKey,
		daemonTransactionRequired: true,
	}
	// The API handler outlives individual source-order/enabled updates. Read the
	// authoritative config file for every action instead of retaining cfg's
	// startup snapshot in sourceEnabled.
	runtime.sourceEnabledFn = func() (bool, error) {
		current, err := loadConfig()
		if err != nil {
			return false, err
		}
		return office91SourceEnabledInConfig(current), nil
	}
	return runtime
}

func office91SourceEnabledInConfig(cfg Config) bool {
	if !cfg.Office91Enabled {
		return false
	}
	for _, source := range cfg.Sources {
		if source.ID == sourceIDOffice91 && source.Type == SourceTypeSubscription {
			return source.Enabled
		}
	}
	return false
}

func (s *office91OfflineRuntime) sourceEnabledNow() (bool, error) {
	if s.sourceEnabledFn != nil {
		return s.sourceEnabledFn()
	}
	return s.sourceEnabled, nil
}

func (s *office91OfflineRuntime) ensureSourceEnabled() error {
	enabled, err := s.sourceEnabledNow()
	if err != nil || !enabled {
		return errors.New("offline-source-disabled")
	}
	return nil
}

func (s *office91OfflineRuntime) ensureDaemonTransaction() error {
	if s.daemonTransactionRequired && office91OfflineTransactionHook() == nil {
		return errors.New("offline-runtime-not-ready")
	}
	return nil
}

func (s *office91OfflineRuntime) Running() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.running }

func (s *office91OfflineRuntime) Snapshot() SubscriptionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

type office91FileSnapshot struct {
	path   string
	exists bool
	mode   os.FileMode
	data   []byte
}

func snapshotOffice91File(path string) (office91FileSnapshot, error) {
	snapshot := office91FileSnapshot{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return snapshot, errors.New("offline-snapshot-unavailable")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return snapshot, errors.New("offline-snapshot-unavailable")
	}
	snapshot.exists = true
	snapshot.mode = info.Mode().Perm()
	snapshot.data = b
	return snapshot, nil
}

// restoreOffice91File uses a same-directory private temporary and rename, so
// rollback cannot expose a partially-written engine or source file.
func restoreOffice91File(snapshot office91FileSnapshot) error {
	if !snapshot.exists {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("offline-rollback-failed")
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snapshot.path), 0700); err != nil {
		return errors.New("offline-rollback-failed")
	}
	tmp, err := os.CreateTemp(filepath.Dir(snapshot.path), ".office91-rollback-")
	if err != nil {
		return errors.New("offline-rollback-failed")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	err = tmp.Chmod(snapshot.mode.Perm())
	if err == nil {
		_, err = tmp.Write(snapshot.data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("offline-rollback-failed")
	}
	if err := os.Rename(tmpPath, snapshot.path); err != nil {
		return errors.New("offline-rollback-failed")
	}
	return nil
}

func snapshotOffice91Transaction() ([]office91FileSnapshot, office91ModeSnapshot, error) {
	mode, err := snapshotOffice91Mode()
	if err != nil {
		return nil, office91ModeSnapshot{}, err
	}
	paths := []string{
		subscriptionYAMLPath(sourceIDOffice91),
		engineConfigPath(),
		nodeLabelsPath(),
	}
	files := make([]office91FileSnapshot, 0, len(paths))
	for _, path := range paths {
		file, err := snapshotOffice91File(path)
		if err != nil {
			return nil, office91ModeSnapshot{}, err
		}
		files = append(files, file)
	}
	return files, mode, nil
}

func restoreOffice91Transaction(files []office91FileSnapshot, mode office91ModeSnapshot) error {
	var first error
	for _, file := range files {
		if err := restoreOffice91File(file); err != nil && first == nil {
			first = err
		}
	}
	if err := restoreOffice91Mode(mode); err != nil && first == nil {
		first = err
	}
	if first != nil {
		return errors.New("offline-rollback-failed")
	}
	return nil
}

func (s *office91OfflineRuntime) resolve(req office91OfflineRequest) (string, string, string, string, error) {
	if err := s.ensureSourceEnabled(); err != nil {
		return "", "", "", "", err
	}
	profile, key, python, validator := req.ProfilePath, req.KeyPath, req.PythonPath, req.ValidatorPath
	if key == "" {
		key = s.defaultKey
	}
	if python == "" {
		python = s.defaultPython
	}
	if validator == "" {
		validator = s.defaultEngine
	}
	for name, path := range map[string]string{
		"profile": profile, "key": key, "python": python, "validator": validator,
	} {
		if path != "" && !filepath.IsAbs(path) {
			return "", "", "", "", fmt.Errorf("offline-%s-requires-absolute-path", name)
		}
	}
	if key == "" {
		return "", "", "", "", errors.New("offline-key-unavailable")
	}
	if _, err := os.Lstat(key); err != nil {
		canInitialize := errors.Is(err, os.ErrNotExist) && req.KeyPath == "" && s.bootstrap != nil && filepath.IsAbs(s.bootstrapPath)
		if !canInitialize {
			return "", "", "", "", errors.New("offline-key-unavailable")
		}
	}
	if python == "" {
		return "", "", "", "", errors.New("offline-python-unavailable")
	}
	if validator == "" {
		return "", "", "", "", errors.New("offline-validator-unavailable")
	}
	return profile, key, python, validator, nil
}

func (s *office91OfflineRuntime) markAttempt() {
	now := time.Now().UTC()
	s.mu.Lock()
	s.status.SourceID = sourceIDOffice91
	s.status.LastAttempt = &now
	s.status.Status = "running"
	s.status.Error = ""
	s.mu.Unlock()
	recordOffice91OfflineStatus(s.Snapshot())
}

func (s *office91OfflineRuntime) finish(err error) {
	now := time.Now().UTC()
	s.mu.Lock()
	s.status.SourceID = sourceIDOffice91
	if err == nil {
		s.status.Status = "ok"
		s.status.Error = ""
		s.status.LastSuccess = &now
	} else {
		s.status.Status = "error"
		s.status.Error = office91OfflineErrorClass(err)
		// LastSuccess, Proxies, and Rules remain unchanged on failure. The
		// importer has already preserved the last valid cache.
	}
	status := s.status
	s.mu.Unlock()
	recordOffice91OfflineStatus(status)
}

// refresh is synchronous for deterministic callers and tests. HTTP callers
// use enqueue so request cancellation cannot kill an accepted import.
func (s *office91OfflineRuntime) refresh(ctx context.Context, req office91OfflineRequest) error {
	if err := s.ensureSourceEnabled(); err != nil {
		return err
	}
	if err := s.ensureDaemonTransaction(); err != nil {
		return err
	}
	s.markAttempt()
	if ctx == nil {
		ctx = context.Background()
	}
	if transaction := office91OfflineTransactionHook(); transaction != nil {
		err := transaction(ctx, func(activeCtx context.Context, apply, rollback func() error) error {
			return s.refreshLocked(activeCtx, req, apply, rollback)
		})
		if err != nil {
			s.finish(err)
		}
		return err
	}
	unlock, err := acquireSourceProcessLock(ctx)
	if err != nil {
		s.finish(err)
		return err
	}
	defer unlock()
	apply, rollback := office91OfflineApplyHooks()
	return s.refreshLocked(ctx, req, apply, rollback)
}
func (s *office91OfflineRuntime) refreshLocked(ctx context.Context, req office91OfflineRequest, apply, rollback func() error) error {
	// The lock covers every writer transition. An online refresher that is
	// already fetching may finish its network request, but it cannot publish a
	// node cache after this transaction marks the source offline.
	office91SourceWriteMu.Lock()
	defer office91SourceWriteMu.Unlock()
	if err := ctx.Err(); err != nil {
		s.finish(err)
		return err
	}
	profile, key, python, validator, err := s.resolve(req)
	if err != nil {
		s.finish(err)
		return err
	}
	if _, keyErr := os.Lstat(key); errors.Is(keyErr, os.ErrNotExist) && req.KeyPath == "" {
		if s.bootstrap == nil {
			s.finish(errors.New("offline-key-unavailable"))
			return errors.New("offline-key-unavailable")
		}
		if err := s.bootstrap(ctx, key, s.bootstrapPath, python); err != nil {
			s.finish(err)
			return err
		}
	}
	files, mode, err := snapshotOffice91Transaction()
	if err != nil {
		s.finish(err)
		return err
	}
	if err := writeOffice91Mode(office91ModeOffline); err != nil {
		s.finish(err)
		return err
	}
	if s.importer == nil {
		err = errors.New("offline-importer-unavailable")
		if rollbackErr := restoreOffice91Transaction(files, mode); rollbackErr != nil {
			err = rollbackErr
		}
		s.finish(err)
		return err
	}
	err = s.importer(ctx, profile, key, python, validator)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		if rollbackErr := restoreOffice91Transaction(files, mode); rollbackErr != nil {
			err = rollbackErr
		}
		s.finish(err)
		return err
	}
	if apply != nil {
		applyErr := apply()
		if applyErr == nil {
			applyErr = ctx.Err()
		}
		if applyErr != nil {
			rollbackErr := restoreOffice91Transaction(files, mode)
			if rollbackErr == nil && rollback != nil {
				rollbackErr = rollback()
			}
			if rollbackErr != nil {
				err = errors.New("offline-rollback-failed")
			} else if errors.Is(applyErr, context.Canceled) || errors.Is(applyErr, context.DeadlineExceeded) {
				err = applyErr
			} else {
				err = errors.New("offline-reload-failed")
			}
			s.finish(err)
			return err
		}
	} else {
		// A command-line/isolated caller has no engine to reload. Keep the
		// authenticated cache and notify a best-effort local observer only.
		notifyOffice91OfflineUpdate()
		s.mu.Lock()
		s.status.Status = "cached"
		s.status.Error = ""
		status := s.status
		s.mu.Unlock()
		recordOffice91OfflineStatus(status)
		return nil
	}
	s.finish(nil)
	return nil
}

// enqueue starts at most one explicit import at a time. A second request is
// acknowledged as already running rather than starting a concurrent writer.
func (s *office91OfflineRuntime) enqueue(req office91OfflineRequest) (bool, error) {
	if _, _, _, _, err := s.resolve(req); err != nil {
		return false, err
	}
	if err := s.ensureDaemonTransaction(); err != nil {
		return false, err
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, nil
	}
	s.running = true
	timeout := s.timeout
	s.mu.Unlock()
	s.markAttempt()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_ = s.refresh(ctx, req)
	}()
	return true, nil
}

// switchOnline is the explicit escape hatch from persistent offline mode. It
// never claims that an online refresh ran unless the source runtime accepted
// its wake-up request; the mode marker itself remains durable across restart.
func (s *office91OfflineRuntime) switchOnline() (bool, error) {
	if err := s.ensureSourceEnabled(); err != nil {
		return false, err
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		return false, errors.New("offline-import-in-progress")
	}
	office91SourceWriteMu.Lock()
	defer office91SourceWriteMu.Unlock()
	if err := s.ensureSourceEnabled(); err != nil {
		return false, err
	}
	if err := writeOffice91Mode(office91ModeOnline); err != nil {
		return false, err
	}
	return requestSourceRefresh(sourceIDOffice91), nil
}

func office91OfflineErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	msg := err.Error()
	for _, class := range []string{
		"offline-profile-unavailable",
		"offline-profile-identity-ambiguous",
		"offline-source-disabled",
		"offline-mode-invalid",
		"offline-mode-unavailable",
		"offline-snapshot-unavailable",
		"offline-mode-rollback-failed",
		"offline-rollback-failed",
		"offline-reload-failed",
		"offline-runtime-unavailable",
		"offline-runtime-not-ready",
		"offline-generation-failed",
		"offline-importer-unavailable",
		"offline-import-in-progress",
		"offline-key-unavailable",
		"offline-key-bootstrap-unavailable",
		"offline-key-bootstrap-identity-mismatch",
		"offline-key-bootstrap-failed",
		"offline-python-unavailable",
		"offline-validator-unavailable",
		"offline-import-requires-absolute-paths",
		"offline-key-requires-absolute-path",
		"offline-profile-requires-absolute-path",
		"offline-python-requires-absolute-path",
		"offline-validator-requires-absolute-path",
	} {
		if msg == class {
			return class
		}
	}
	if strings.HasPrefix(msg, "offline-") {
		return "offline-authentication-or-validation-failed"
	}
	return "offline-authentication-or-validation-failed"
}
