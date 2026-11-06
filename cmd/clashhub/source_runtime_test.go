package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProfileStoreDoesNotOverwriteSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "123.yaml")
	original := []byte("original profile")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	r := newProfileRefresher(SourceConfig{ID: "test", Path: path}, time.Hour)
	r.userinfoPath = filepath.Join(dir, "userinfo")
	r.validate = func([]byte) (int, int, error) { return 1, 0, nil }
	if _, _, err := r.storeProfile([]byte("replacement"), "total=100"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatal("source profile changed")
	}
	info, ok := readProfileUserinfo(r.userinfoPath)
	if !ok || info.ProfileID != "123" || info.Total == nil || *info.Total != 100 {
		t.Fatal("userinfo cache missing or not bound to profile")
	}
}

func TestDefaultJiuyiSignerFailsClosed(t *testing.T) {
	home := fakeHome(t)
	cfg := defaultConfig()
	cfg.Office91Enabled = true
	cfg.Sources = append(cfg.Sources, SourceConfig{ID: sourceIDOffice91, Label: "Office91", Type: SourceTypeSubscription, Enabled: true})
	writeConfigFile(t, home, cfg)
	r := newSubscriptionRefresher(SourceConfig{ID: sourceIDOffice91}, time.Hour)
	r.resolveURL = func() (string, error) { return "https://example.invalid/sub", nil }
	r.RefreshOnce(context.Background())
	if got := r.Snapshot(); got.Status != "error" || got.Error != "signing-unverified" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestSubscriptionClientRejectsRedirects(t *testing.T) {
	c := directSubscriptionClient(time.Second)
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
}

func TestSubscriptionStatusRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // Registry defaults must not depend on the user's persisted source mode.
	recordSubscriptionStatus(SubscriptionStatus{SourceID: "office91", Status: "ok", Proxies: 7})
	if st, ok := subscriptionStatusSnapshot("office91"); !ok || st.Status != "ok" || st.Proxies != 7 {
		t.Fatalf("status not recorded: %+v ok=%v", st, ok)
	}
	// Empty source ids must not create entries.
	recordSubscriptionStatus(SubscriptionStatus{Status: "ok"})
	if _, ok := subscriptionStatusSnapshot(""); ok {
		t.Fatal("empty source id was recorded")
	}
	if _, ok := subscriptionStatusSnapshot("missing"); ok {
		t.Fatal("unknown source id reported present")
	}
}

func TestRunSourceSubscriptionsWithoutSubscriptions(t *testing.T) {
	// A nil source list is the documented fast path. A non-nil list starts the
	// long-lived profile/subscription refresh runtime and is expected to remain
	// alive until its context is cancelled.
	app := &App{cfg: Config{Sources: nil}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runSourceSubscriptions(ctx)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("runSourceSubscriptions blocked with no subscription sources")
	}
	cancel()
}
