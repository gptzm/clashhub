package main

// Cross-verification of the reconstructed 玖壹 signature scheme against the
// reference Python signer (reverse/work/verify/signer.py in the reverse
// engineering workspace). The tests below re-implement the Python reference
// exactly: HMAC-SHA256 over "\n".join(component, body, ts, nonce_hex,
// component), key = seed expanded with (b ^ ((i*17+0x5a)&0xff)), X-Ts unix
// seconds, X-Nonce lower-case hex of 16 random bytes. All vectors use
// throwaway inputs; the real key is never embedded in test expectations.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pySignerSign replicates reverse/work/verify/signer.py byte for byte.
func pySignerSign(key []byte, a0, body string, ts int64, nonce []byte) string {
	nonceHex := hex.EncodeToString(nonce)
	msg := a0 + "\n" + body + "\n" + strconv.FormatInt(ts, 10) + "\n" + nonceHex + "\n" + a0
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestSignedSubscriptionHeadersMatchesPythonSigner cross-checks the Go header
// builder against the Python reference semantics on fixed vectors.
func TestSignedSubscriptionHeadersMatchesPythonSigner(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
	fixed := time.Unix(1761800000, 0)
	nonce := func() []byte {
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}
	h, err := signedSubscriptionHeaders(key, "/api/v1/guest/comm/config", "", func() time.Time { return fixed }, nonce)
	if err != nil {
		t.Fatalf("signedSubscriptionHeaders: %v", err)
	}
	if got := h.Get("X-Ts"); got != "1761800000" {
		t.Fatalf("X-Ts = %q, want 1761800000", got)
	}
	if got := h.Get("X-Nonce"); got != "000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("X-Nonce = %q", got)
	}
	want := pySignerSign(key, "/api/v1/guest/comm/config", "", 1761800000, nonce())
	if got := h.Get("X-Sign"); got != want {
		t.Fatalf("X-Sign mismatch: Go %q != Python-reference %q", got, want)
	}
	if len(h.Get("X-Sign")) != 64 {
		t.Fatalf("X-Sign length = %d, want 64 hex chars", len(h.Get("X-Sign")))
	}
}

// TestSignedSubscriptionHeadersBodyInSign verifies the body participates in
// the message for POST-shaped components too.
func TestSignedSubscriptionHeadersBodyInSign(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	fixed := time.Unix(1761800001, 0)
	nb := []byte{15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0}
	h, err := signedSubscriptionHeaders(key, "https://example.test/sub", "k=v", func() time.Time { return fixed }, func() []byte { return nb })
	if err != nil {
		t.Fatalf("signedSubscriptionHeaders: %v", err)
	}
	want := pySignerSign(key, "https://example.test/sub", "k=v", 1761800001, nb)
	if got := h.Get("X-Sign"); got != want {
		t.Fatalf("X-Sign mismatch with body: Go %q != reference %q", got, want)
	}
}

// TestSignedSubscriptionHeadersBadNonce ensures the contract surfaces a bad
// nonce provider instead of silently sending unsigned headers.
func TestSignedSubscriptionHeadersBadNonce(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	if _, err := signedSubscriptionHeaders(key, "/p", "", time.Now, func() []byte { return make([]byte, 8) }); err == nil {
		t.Fatal("expected error for short nonce, got nil")
	}
}

// TestSubscriptionRefresherSendsSignedHeaders runs a full RefreshOnce against
// an httptest server and asserts the request carried the three signed headers
// plus the app-shaped UA, with X-Sign matching the Python reference computed
// from the observed ts/nonce values.
func TestSubscriptionRefresherSendsSignedHeaders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	enableOffice91ModuleForTest(t)
	var gotHeader http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotPath = r.URL.String()
		w.Header().Set("subscription-userinfo", "upload=1; download=2; total=300; expire=1761900000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxies: [{name: n1, server: s1, port: 1, type: ss}]\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	ref := &subscriptionRefresher{
		sourceID:     "office91",
		label:        "test",
		urlPath:      dir + "/office91.url",
		yamlPath:     dir + "/office91.yaml",
		userinfoPath: dir + "/office91.userinfo",
		interval:     time.Hour,
		fetchTimeout: 5 * time.Second,
		client:       srv.Client(),
		now:          func() time.Time { return time.Unix(1761800002, 0) },
		status:       "unknown",
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	ref.signHeaders = func(component, body string) (http.Header, error) {
		return signedSubscriptionHeaders(key, component, body, ref.now, func() []byte {
			b := make([]byte, 16)
			for i := range b {
				b[i] = byte(0xa0 + i)
			}
			return b
		})
	}
	ref.resolveURL = func() (string, error) { return srv.URL + "/api/v1/client/subscribe", nil }
	ref.parseStore = func(body []byte, dst string) (int, int, error) { return 1, 0, nil }

	ref.RefreshOnce(t.Context())
	if st := ref.Snapshot(); st.Status != "ok" {
		t.Fatalf("status = %q (%s), want ok", st.Status, st.Error)
	}
	for _, k := range []string{"X-Ts", "X-Nonce", "X-Sign"} {
		if gotHeader.Get(k) == "" {
			t.Fatalf("request missing %s header", k)
		}
	}
	if gotHeader.Get("User-Agent") != jiuyiUserAgent {
		t.Fatalf("User-Agent = %q, want app-shaped UA", gotHeader.Get("User-Agent"))
	}
	ts, err := strconv.ParseInt(gotHeader.Get("X-Ts"), 10, 64)
	if err != nil {
		t.Fatalf("X-Ts not an integer: %v", err)
	}
	nonceHex := gotHeader.Get("X-Nonce")
	if len(nonceHex) != 32 || strings.ToLower(nonceHex) != nonceHex {
		t.Fatalf("X-Nonce = %q, want 32 lower-case hex chars", nonceHex)
	}
	nonce, derr := hex.DecodeString(nonceHex)
	if derr != nil {
		t.Fatalf("X-Nonce not hex: %v", derr)
	}
	want := pySignerSign(key, srv.URL+"/api/v1/client/subscribe", "", ts, nonce)
	if got := gotHeader.Get("X-Sign"); got != want {
		t.Fatalf("server-observed X-Sign mismatch: %q != %q", got, want)
	}
	if gotPath != "/api/v1/client/subscribe" {
		t.Fatalf("path = %q", gotPath)
	}

	// The userinfo cache must mirror the official quota with fetched_at.
	b, rerr := os.ReadFile(ref.userinfoPath)
	if rerr != nil {
		t.Fatalf("userinfo cache missing: %v", rerr)
	}
	if !strings.Contains(string(b), `"fetched_at":1761800002`) || !strings.Contains(string(b), `"total":300`) {
		t.Fatalf("userinfo cache content unexpected: %q", string(b))
	}
}

// TestSubscriptionRefresherUnsignedOnNilSigner documents that a nil signer
// still produces a well-formed (unsigned) request rather than a crash.
func TestSubscriptionRefresherUnsignedOnNilSigner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	enableOffice91ModuleForTest(t)
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxies: [{name: n1, server: s1, port: 1, type: ss}]\n"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	ref := &subscriptionRefresher{
		sourceID:    "office91",
		yamlPath:    dir + "/office91.yaml",
		interval:    time.Hour,
		client:      srv.Client(),
		now:         time.Now,
		status:      "unknown",
		signHeaders: nil,
		resolveURL:  func() (string, error) { return srv.URL + "/s", nil },
		parseStore:  func(body []byte, dst string) (int, int, error) { return 1, 0, nil },
	}
	ref.RefreshOnce(t.Context())
	if gotUA != jiuyiUserAgent {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if st := ref.Snapshot(); st.Status != "ok" {
		t.Fatalf("status = %q", st.Status)
	}
}
