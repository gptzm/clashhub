package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDomainValidation(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ExAmPlE.COM.", "example.com"}, {"a-b.example", "a-b.example"}, {"localhost", "localhost"}, {"xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"127.0.0.1", ""}, {"127.1", ""}, {"::1", ""}, {"[::1]", ""}, {"*.example.com", ""}, {"https://example.com", ""}, {"example.com:80", ""}, {" example.com", ""}, {"foo..com", ""}, {"-foo.com", ""}, {"foo-.com", ""}, {"foo.com..", ""}, {"bücher.de", ""}, {strings.Repeat("a", 64) + ".com", ""}, {strings.Repeat("a.", 127) + "com", ""},
	} {
		got, err := normalizeDomain(tc.in)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%q accepted as %q", tc.in, got)
			}
		} else if err != nil || got != tc.want {
			t.Errorf("%q got %q %v", tc.in, got, err)
		}
	}
}

func TestBlockedDomainsAPIAndPersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: Config{HTTPListen: "127.0.0.1:33333"}}
	h, err := a.nodeAPIHandler()
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(appSupportDir(), "api-token"))
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, body, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:33333/api/blocked-domains", strings.NewReader(body))
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Content-Type", "application/json")
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	tk := strings.TrimSpace(string(token))
	if w := request("GET", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("PUT", `{"domains":["B.COM.","a.com","b.com"]}`, tk); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"domains":["a.com","b.com"]}` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	st, err := os.Stat(blockedDomainPath())
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v %v", st, err)
	}
	for _, body := range []string{`{"domains":["127.0.0.1"]}`, `{"domains":null}`, `{"domains":["ok.com"],"x":1}`, `{"domains":[]} {}`} {
		if w := request("PUT", body, tk); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	m, err := readBlockedDomains(blockedDomainPath())
	if err != nil || len(m) != 2 {
		t.Fatalf("reload %v %v", m, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := request("POST", fmt.Sprintf(`{"domain":"d%d.example"}`, i), tk)
			if w.Code != 200 {
				t.Errorf("post %d", w.Code)
			}
		}(i)
	}
	wg.Wait()
	w := request("GET", "", tk)
	var result blockedDomainList
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Domains) != 22 {
		t.Fatalf("concurrency %s %v", w.Body, err)
	}
	if w = request("DELETE", `{"domain":"A.COM."}`, tk); w.Code != 200 || a.domainBlocked("a.com") {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}
	if w = request("PUT", `{"domains":[]}`, tk); w.Code != 200 || a.domainBlocked("b.com") {
		t.Fatalf("clear %s", w.Body)
	}
}

func TestBlockedProxyPaths(t *testing.T) {
	a := &App{blockedDomains: map[string]struct{}{"example.com": {}}, mode: ModeProbing, probeEnd: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		method, url string
		upgrade     bool
	}{{"CONNECT", "http://example.com:443", false}, {"GET", "http://EXAMPLE.COM./", false}, {"GET", "http://example.com/socket", true}} {
		r := httptest.NewRequest(tc.method, tc.url, nil)
		if tc.method == "CONNECT" {
			r.Host = "example.com:443"
		}
		if tc.upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%s got %d", tc.method, w.Code)
		}
	}
	if a.domainBlocked("sub.example.com") || a.domainBlocked("127.0.0.1") {
		t.Fatal("unexpected match")
	}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { a.handleSOCKS(server); close(done) }()
	defer client.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	client.Write([]byte{5, 1, 0})
	reply := make([]byte, 2)
	io.ReadFull(client, reply)
	host := []byte("example.com")
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = append(req, 0, 80)
	client.Write(req)
	response := make([]byte, 10)
	if _, err := io.ReadFull(client, response); err != nil || !bytes.Equal(response[:2], []byte{5, 2}) {
		t.Fatalf("socks %v %v", response, err)
	}
	<-done
	if a.mode != ModeProbing {
		t.Fatal("blocked request changed mode")
	}
}

func TestBlockedWriteFailureKeepsRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{blockedDomains: map[string]struct{}{"old.example": {}}}
	r := httptest.NewRequest("PUT", "/api/blocked-domains", strings.NewReader(`{"domains":["new.example"]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.serveBlockedDomains(w, r)
	if w.Code != 500 || !a.domainBlocked("old.example") || a.domainBlocked("new.example") {
		t.Fatalf("failure changed rules: %d", w.Code)
	}
}
