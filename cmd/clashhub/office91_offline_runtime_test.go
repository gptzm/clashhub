package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func isolatedOffice91OfflineRuntime(t *testing.T, importer func(context.Context, string, string, string, string) error) (*office91OfflineRuntime, office91OfflineRequest) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	key := filepath.Join(appSupportDir(), "secrets", "office91.key")
	if err := os.MkdirAll(filepath.Dir(key), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	return &office91OfflineRuntime{
			status:        SubscriptionStatus{SourceID: sourceIDOffice91, Status: "unknown"},
			sourceEnabled: true,
			defaultKey:    key,
			defaultPython: filepath.Join(d, "python3"),
			defaultEngine: filepath.Join(d, "mihomo"),
			timeout:       time.Second,
			importer:      importer,
		}, office91OfflineRequest{
			ProfilePath:   filepath.Join(d, "profile.yaml"),
			KeyPath:       key,
			PythonPath:    filepath.Join(d, "python3"),
			ValidatorPath: filepath.Join(d, "mihomo"),
		}
}

func TestOffice91OfflineRuntimeFailureKeepsLastCache(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		return errors.New("offline-profile-authentication-or-validation-failed")
	})
	target := filepath.Join(appSupportDir(), "sources", "office91.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	old := []byte("proxies:\n- name: known-good\n")
	if err := os.WriteFile(target, old, 0600); err != nil {
		t.Fatal(err)
	}

	if err := runtime.refresh(context.Background(), request); err == nil {
		t.Fatal("authentication failure accepted")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("last good cache changed: %q %v", got, err)
	}
	status := runtime.Snapshot()
	if status.Status != "error" || status.Error != "offline-authentication-or-validation-failed" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.LastSuccess != nil {
		t.Fatal("failed import recorded a success")
	}
	if got, ok := subscriptionStatusSnapshot(sourceIDOffice91); !ok || got.Error != status.Error {
		t.Fatalf("status registry not updated: %+v %v", got, ok)
	}
	if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
		t.Fatalf("failed import left offline mode marker: %v", err)
	}
}

func TestOffice91OfflineRuntimeSuccessNotifiesReload(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		return nil
	})
	updated := make(chan struct{})
	setOffice91OfflineUpdate(func() { close(updated) })
	t.Cleanup(func() { setOffice91OfflineUpdate(nil) })

	if err := runtime.refresh(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	status := runtime.Snapshot()
	if status.Status != "cached" || status.LastSuccess != nil || status.Error != "" {
		t.Fatalf("unexpected success status: %+v", status)
	}
	mode, err := readOffice91Mode()
	if err != nil || mode != office91ModeOffline {
		t.Fatalf("successful import did not persist offline mode: %q %v", mode, err)
	}
	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("successful import did not notify reload path")
	}
}

func TestOffice91OfflineRuntimeEnqueueSingleflight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		close(started)
		<-release
		return nil
	})
	queued, err := runtime.enqueue(request)
	if err != nil || !queued {
		t.Fatalf("first request not queued: %v %v", queued, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("import did not start")
	}
	queued, err = runtime.enqueue(request)
	if err != nil || queued {
		t.Fatalf("concurrent request was not coalesced: %v %v", queued, err)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		runtime.mu.Lock()
		running := runtime.running
		runtime.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("singleflight import did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if status := runtime.Snapshot(); status.Status != "cached" {
		t.Fatalf("import did not finish successfully: %+v", status)
	}
}

func TestOffice91OfflineAPIAcceptsExplicitImportOnlyForOffice91(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		return nil
	})
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	c := &subscriptionCache{offline: runtime}
	body, err := json.Marshal(struct {
		SourceID      string `json:"source_id"`
		Mode          string `json:"mode"`
		ProfilePath   string `json:"profile_path"`
		KeyPath       string `json:"key_path"`
		PythonPath    string `json:"python_path"`
		ValidatorPath string `json:"validator_path"`
	}{sourceIDOffice91, "offline", request.ProfilePath, request.KeyPath, request.PythonPath, request.ValidatorPath})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/subscriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.serveHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("offline import not accepted: %d %s", w.Code, w.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response["status"] != "queued" || response["source_id"] != sourceIDOffice91 {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}

	badBody := bytes.NewBufferString(`{"source_id":"big-airport","mode":"offline"}`)
	bad := httptest.NewRequest(http.MethodPost, "/api/subscriptions", badBody)
	bad.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	c.serveHTTP(w, bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("non-office91 offline request accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestOffice91OfflineDisabledSourceIsRejected(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		return nil
	})
	runtime.sourceEnabled = false
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	c := &subscriptionCache{offline: runtime}
	body, err := json.Marshal(map[string]string{
		"source_id": sourceIDOffice91, "mode": "offline",
		"profile_path": request.ProfilePath, "key_path": request.KeyPath,
		"python_path": request.PythonPath, "validator_path": request.ValidatorPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/subscriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.serveHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("disabled source accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestOffice91OfflineRuntimeReadsCurrentSourceConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	runtime := newOffice91OfflineRuntime(cfg)
	key := filepath.Join(appSupportDir(), "secrets", "office91.key")
	if err := os.MkdirAll(filepath.Dir(key), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime.defaultKey = key
	runtime.defaultPython = filepath.Join(t.TempDir(), "python3")
	runtime.defaultEngine = filepath.Join(t.TempDir(), "mihomo")
	request := office91OfflineRequest{ProfilePath: filepath.Join(t.TempDir(), "profile.yaml")}
	if _, _, _, _, err := runtime.resolve(request); err != nil {
		t.Fatalf("enabled source rejected: %v", err)
	}

	for i := range cfg.Sources {
		if cfg.Sources[i].ID == sourceIDOffice91 {
			cfg.Sources[i].Enabled = false
		}
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := runtime.resolve(request); err == nil || office91OfflineErrorClass(err) != "offline-source-disabled" {
		t.Fatalf("disabled source was accepted: %v", err)
	}

	for i := range cfg.Sources {
		if cfg.Sources[i].ID == sourceIDOffice91 {
			cfg.Sources[i].Enabled = true
		}
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := runtime.resolve(request); err != nil {
		t.Fatalf("re-enabled source remained rejected: %v", err)
	}
}

func TestOffice91OfflineRuntimeRechecksSourceInsideTransaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		t.Fatal("disabled source reached importer")
		return nil
	})
	runtime.sourceEnabledFn = func() (bool, error) {
		current, err := loadConfig()
		if err != nil {
			return false, err
		}
		return office91SourceEnabledInConfig(current), nil
	}
	setOffice91OfflineTransaction(func(ctx context.Context, body func(context.Context, func() error, func() error) error) error {
		current, err := loadConfig()
		if err != nil {
			return err
		}
		for i := range current.Sources {
			if current.Sources[i].ID == sourceIDOffice91 {
				current.Sources[i].Enabled = false
			}
		}
		if err := saveConfig(current); err != nil {
			return err
		}
		return body(ctx, nil, nil)
	})
	t.Cleanup(func() { setOffice91OfflineTransaction(nil) })
	if err := runtime.refresh(context.Background(), request); err == nil || office91OfflineErrorClass(err) != "offline-source-disabled" {
		t.Fatalf("transaction accepted a source disabled after preflight: %v", err)
	}
}

func TestOffice91OfflineAPIRejectsBeforeDaemonTransactionReady(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		t.Fatal("import started without daemon transaction")
		return nil
	})
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	runtime.daemonTransactionRequired = true
	c := &subscriptionCache{offline: runtime}
	body, err := json.Marshal(map[string]string{
		"source_id": sourceIDOffice91, "mode": "offline",
		"profile_path": request.ProfilePath, "key_path": request.KeyPath,
		"python_path": request.PythonPath, "validator_path": request.ValidatorPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/subscriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.serveHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !bytes.Contains(w.Body.Bytes(), []byte("offline-runtime-not-ready")) {
		t.Fatalf("not-ready runtime was accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestOffice91OnlineRefresherCannotWritePersistentOfflineCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := writeOffice91Mode(office91ModeOffline); err != nil {
		t.Fatal(err)
	}
	r := newSubscriptionRefresher(SourceConfig{ID: sourceIDOffice91, Type: SourceTypeSubscription, Enabled: true}, time.Hour)
	called := false
	r.resolveURL = func() (string, error) {
		t.Fatal("offline mode attempted an online URL read")
		return "", nil
	}
	r.parseStore = func([]byte, string) (int, int, error) {
		called = true
		return 1, 0, nil
	}
	r.RefreshOnce(context.Background())
	if called {
		t.Fatal("online refresher wrote while offline mode was persistent")
	}
	if status := r.Snapshot(); status.Status != "error" || status.Error != "offline-mode" {
		t.Fatalf("unexpected blocked status: %+v", status)
	}
}

func TestOffice91OfflineRuntimeRollsBackCacheAndConfigOnReloadFailure(t *testing.T) {
	oldCache := []byte("proxies:\n- name: old\n")
	oldConfig := []byte("secret: old\nproxies: []\n")
	oldLabels := []byte(`{"_generation":"old"}`)
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		if err := os.WriteFile(subscriptionYAMLPath(sourceIDOffice91), []byte("proxies:\n- name: new\n"), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(engineConfigPath(), []byte("secret: new\nproxies: [new]\n"), 0600); err != nil {
			return err
		}
		return os.WriteFile(nodeLabelsPath(), []byte(`{"_generation":"new"}`), 0600)
	})
	if err := os.MkdirAll(filepath.Dir(subscriptionYAMLPath(sourceIDOffice91)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(engineConfigPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subscriptionYAMLPath(sourceIDOffice91), oldCache, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), oldConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeLabelsPath(), oldLabels, 0600); err != nil {
		t.Fatal(err)
	}
	rollbackCalled := false
	setOffice91OfflineApply(func() error { return errors.New("engine-reload-failed") }, func() error {
		rollbackCalled = true
		return nil
	})
	t.Cleanup(func() { setOffice91OfflineApply(nil, nil) })
	if err := runtime.refresh(context.Background(), request); err == nil {
		t.Fatal("reload failure was accepted")
	}
	if !rollbackCalled {
		t.Fatal("old engine config was not offered to rollback")
	}
	for _, check := range []struct {
		path string
		want []byte
	}{
		{subscriptionYAMLPath(sourceIDOffice91), oldCache},
		{engineConfigPath(), oldConfig},
		{nodeLabelsPath(), oldLabels},
	} {
		got, err := os.ReadFile(check.path)
		if err != nil || !bytes.Equal(got, check.want) {
			t.Fatalf("%s not rolled back: %q %v", check.path, got, err)
		}
	}
	if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
		t.Fatalf("reload failure left offline mode marker: %v", err)
	}
	if status := runtime.Snapshot(); status.Status != "error" || status.Error != "offline-reload-failed" {
		t.Fatalf("unexpected reload failure status: %+v", status)
	}
}

func TestOffice91TransactionSerializesSnapshotThroughRollback(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		close(started)
		<-release
		return os.WriteFile(subscriptionYAMLPath(sourceIDOffice91), []byte("new-cache"), 0600)
	})
	if err := os.MkdirAll(filepath.Dir(subscriptionYAMLPath(sourceIDOffice91)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(engineConfigPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), []byte("old-engine"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	rollbackSawOld := false
	reload := func() error {
		b, _ := os.ReadFile(engineConfigPath())
		rollbackSawOld = string(b) == "old-engine"
		return nil
	}
	setOffice91OfflineTransaction(func(ctx context.Context, body func(context.Context, func() error, func() error) error) error {
		return a.withOffice91SourceTransaction(context.Background(), ctx, body, func(context.Context) error {
			os.WriteFile(engineConfigPath(), []byte("uncommitted-engine"), 0600)
			return errors.New("failed-generation")
		}, reload)
	})
	t.Cleanup(func() { setOffice91OfflineTransaction(nil) })
	done := make(chan error, 1)
	go func() { done <- runtime.refresh(context.Background(), request) }()
	<-started
	competitor := make(chan struct{})
	go func() {
		a.sourceRegenMu.Lock()
		defer a.sourceRegenMu.Unlock()
		os.WriteFile(engineConfigPath(), []byte("later-engine"), 0600)
		close(competitor)
	}()
	select {
	case <-competitor:
		close(release)
		t.Fatal("transaction did not hold regeneration lock during import")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("generation failure accepted")
	}
	<-competitor
	if !rollbackSawOld {
		t.Fatal("rollback did not reload restored snapshot")
	}
	b, _ := os.ReadFile(engineConfigPath())
	if string(b) != "later-engine" {
		t.Fatal("rollback overwrote later transaction")
	}
	if runtime.Snapshot().Status == "ok" {
		t.Fatal("failed reload marked active")
	}
}

func TestOffice91TransactionCancellationAndShutdown(t *testing.T) {
	for _, kind := range []string{"canceled-before", "canceled-after-import", "shutdown-before"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a := &App{}
			calls := 0
			reloads := 0
			runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
				calls++
				if kind == "canceled-after-import" {
					cancel()
				}
				return nil
			})
			setOffice91OfflineTransaction(func(req context.Context, body func(context.Context, func() error, func() error) error) error {
				return a.withOffice91SourceTransaction(context.Background(), req, body, func(context.Context) error { return nil }, func() error { reloads++; return nil })
			})
			defer setOffice91OfflineTransaction(nil)
			if kind == "canceled-before" {
				cancel()
			}
			if kind == "shutdown-before" {
				a.shuttingDown.Store(true)
			}
			if err := runtime.refresh(ctx, request); err == nil {
				t.Fatal("canceled/shutdown transaction accepted")
			}
			if kind != "canceled-after-import" && calls != 0 {
				t.Fatal("import ran after cancellation/shutdown")
			}
			if reloads != 0 || runtime.Snapshot().Status != "error" {
				t.Fatal("canceled transaction reloaded or claimed success")
			}
			if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
				t.Fatal("failed transaction retained offline marker")
			}
		})
	}
}

func TestOffice91StatusOKRequiresSuccessfulReload(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error { return nil })
	a := &App{}
	reloads := 0
	setOffice91OfflineTransaction(func(ctx context.Context, body func(context.Context, func() error, func() error) error) error {
		return a.withOffice91SourceTransaction(context.Background(), ctx, body, func(context.Context) error { return nil }, func() error { reloads++; return nil })
	})
	defer setOffice91OfflineTransaction(nil)
	if err := runtime.refresh(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 || runtime.Snapshot().Status != "ok" || runtime.Snapshot().LastSuccess == nil {
		t.Fatal("successful reload not proven")
	}
}

func TestOffice91LifecycleCancelsImporterAndPreservesSnapshot(t *testing.T) {
	started := make(chan struct{})
	runtime, request := isolatedOffice91OfflineRuntime(t, func(ctx context.Context, _ string, _ string, _ string, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	a := &App{}
	setOffice91OfflineTransaction(func(ctx context.Context, body func(context.Context, func() error, func() error) error) error {
		return a.withOffice91SourceTransaction(lifecycle, ctx, body, func(context.Context) error { return nil }, func() error { t.Error("canceled importer must not reload"); return nil })
	})
	defer setOffice91OfflineTransaction(nil)
	done := make(chan error, 1)
	go func() { done <- runtime.refresh(context.Background(), request) }()
	<-started
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("lifecycle cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle did not cancel importer")
	}
	if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
		t.Fatal("canceled import retained mode")
	}
	if status := runtime.Snapshot(); status.Status != "error" || status.Error != "canceled" {
		t.Fatal("canceled import status incorrect")
	}
}

func TestOffice91CancellationAfterGenerationReloadsOldSnapshot(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{}
	reloads := 0
	if err := os.MkdirAll(filepath.Dir(engineConfigPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engineConfigPath(), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	setOffice91OfflineTransaction(func(request context.Context, body func(context.Context, func() error, func() error) error) error {
		return a.withOffice91SourceTransaction(context.Background(), request, body, func(context.Context) error {
			os.WriteFile(engineConfigPath(), []byte("new"), 0600)
			cancel()
			return nil
		}, func() error {
			reloads++
			b, _ := os.ReadFile(engineConfigPath())
			if string(b) != "old" {
				t.Error("reload before old snapshot restored")
			}
			return nil
		})
	})
	defer setOffice91OfflineTransaction(nil)
	if err := runtime.refresh(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation after generation accepted")
	}
	if reloads != 1 || runtime.Snapshot().Status == "ok" {
		t.Fatal("rollback not proven or false success")
	}
}

func TestOffice91TransactionLockWaitCanCancel(t *testing.T) {
	a := &App{}
	a.sourceRegenMu.Lock()
	defer a.sourceRegenMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.withOffice91SourceTransaction(context.Background(), ctx, func(context.Context, func() error, func() error) error {
		t.Error("canceled waiter ran transaction")
		return nil
	}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 300*time.Millisecond {
		t.Fatal("lock waiter did not cancel promptly")
	}
}

func TestOffice91DefaultKeyBootstrapIsExplicitAndOnce(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error { return nil })
	if err := os.Remove(runtime.defaultKey); err != nil {
		t.Fatal(err)
	}
	runtime.bootstrapPath = filepath.Join(t.TempDir(), "frozen-tool.py")
	calls := 0
	runtime.bootstrap = func(ctx context.Context, key, tool, python string) error {
		calls++
		if key != runtime.defaultKey || tool != runtime.bootstrapPath {
			t.Fatal("bootstrap identity changed")
		}
		return os.WriteFile(key, []byte("synthetic-protected-key"), 0600)
	}
	request.KeyPath = ""
	if _, _, _, _, err := runtime.resolve(request); err != nil || calls != 0 {
		t.Fatal("preflight must not initialize key")
	}
	if err := runtime.refresh(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := runtime.refresh(context.Background(), request); err != nil || calls != 1 {
		t.Fatal("default key bootstrap was repeated")
	}
	missing := filepath.Join(t.TempDir(), "custom-key")
	request.KeyPath = missing
	if err := runtime.refresh(context.Background(), request); err == nil || calls != 1 {
		t.Fatal("custom missing key silently initialized")
	}
}

func TestOffice91BootstrapFailurePreservesExistingCache(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		t.Error("import after failed bootstrap")
		return nil
	})
	os.Remove(runtime.defaultKey)
	request.KeyPath = ""
	runtime.bootstrapPath = filepath.Join(t.TempDir(), "untrusted.py")
	runtime.bootstrap = func(context.Context, string, string, string) error {
		return errors.New("offline-key-bootstrap-identity-mismatch")
	}
	if err := os.MkdirAll(sourcesCacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subscriptionYAMLPath(sourceIDOffice91), []byte("last-good"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.refresh(context.Background(), request); err == nil {
		t.Fatal("failed bootstrap accepted")
	}
	got, _ := os.ReadFile(subscriptionYAMLPath(sourceIDOffice91))
	if string(got) != "last-good" {
		t.Fatal("bootstrap failure replaced cache")
	}
	if runtime.Snapshot().Error != "offline-key-bootstrap-identity-mismatch" {
		t.Fatal("bootstrap failure class lost")
	}
	if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
		t.Fatal("failed bootstrap changed mode")
	}
}

func TestOffice91StandaloneImportHonorsProcessLock(t *testing.T) {
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error {
		t.Error("import ran through process lock")
		return nil
	})
	lock, err := acquireSourceProcessLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := runtime.refresh(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("standalone transaction ignored lock cancellation")
	}
	if _, err := os.Stat(office91ModePath()); !os.IsNotExist(err) {
		t.Fatal("waiting import changed mode")
	}
}

func TestOffice91GETExposesModeAndActualRunningState(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	runtime, request := isolatedOffice91OfflineRuntime(t, func(context.Context, string, string, string, string) error { close(started); <-release; return nil })
	c := &subscriptionCache{offline: runtime, updated: time.Now()}
	if queued, err := runtime.enqueue(request); err != nil || !queued {
		t.Fatal(err)
	}
	<-started
	get := func() (string, bool, string) {
		w := httptest.NewRecorder()
		c.serveHTTP(w, httptest.NewRequest("GET", "/api/subscriptions", nil))
		var response struct {
			Mode     string                        `json:"office91_mode"`
			Running  bool                          `json:"office91_offline_running"`
			Statuses map[string]SubscriptionStatus `json:"refresh_statuses"`
		}
		if json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatal("GET decode failed")
		}
		return response.Mode, response.Running, response.Statuses[sourceIDOffice91].Status
	}
	mode, running, status := get()
	if mode != "offline" || !running || status != "running" {
		close(release)
		t.Fatal("running state not truthful")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for runtime.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// A stale in-flight online refresh must not overwrite this offline result.
	recordSubscriptionStatus(SubscriptionStatus{SourceID: sourceIDOffice91, Status: "error", Error: "stale-online-result"})
	mode, running, status = get()
	if mode != "offline" || running || status != "cached" {
		t.Fatal("cache-only state falsely reported reloaded")
	}
}

func TestOffice91GUIDefaultRequestBootstrapsRealProfileIsolated(t *testing.T) {
	if os.Getenv("CLASHHUB_OFFLINE_LIVE") != "1" {
		t.Skip("explicit isolated live fixture only")
	}
	realHome := userHome()
	source, err := discoverOffice91OfflineProfile(filepath.Join(realHome, "Library", "Application Support", "com.abysswalker.office91", "profiles"))
	if err != nil {
		t.Fatal("real source identity unavailable")
	}
	encrypted, err := os.ReadFile(source)
	if err != nil {
		t.Fatal("real fixture unavailable")
	}
	trusted := filepath.Join(realHome, "Library", "Application Support", "ClashHub", "reverse", "work", "verify", "offline_profile_extract.py")
	t.Setenv("HOME", t.TempDir())
	profiles := filepath.Join(userHome(), "Library", "Application Support", "com.abysswalker.office91", "profiles")
	if err := os.MkdirAll(profiles, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profiles, "explicit-fixture.yaml"), encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.EngineBin = "/opt/homebrew/bin/mihomo"
	cfg.Sources = []SourceConfig{{ID: sourceIDOffice91, Label: "offline-test", Type: SourceTypeSubscription, Enabled: true}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	runtime := newOffice91OfflineRuntime(cfg)
	// This fixture intentionally exercises the isolated cache-only path; a
	// daemon-backed production runtime keeps the transaction requirement.
	runtime.daemonTransactionRequired = false
	runtime.bootstrapPath = trusted
	c := &subscriptionCache{offline: runtime, updated: time.Now()}
	req := httptest.NewRequest("POST", "/api/subscriptions", bytes.NewBufferString(`{"source_id":"office91","mode":"offline"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.serveHTTP(w, req)
	if w.Code != 202 {
		t.Fatal("default GUI request rejected")
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if runtime.Running() || runtime.Snapshot().Status != "cached" {
		t.Fatal("default import failed or falsely claimed reload")
	}
	st, err := os.Stat(runtime.defaultKey)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("default bootstrap did not create protected key")
	}
	// Once initialized, importing does not depend on the old reverse tool path.
	runtime.bootstrapPath = filepath.Join(t.TempDir(), "removed-tool.py")
	if err := runtime.refresh(context.Background(), office91OfflineRequest{}); err != nil {
		t.Fatal("existing private key still depended on bootstrap tool")
	}
}
