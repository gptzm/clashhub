package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetrySafeMethods(t *testing.T) {
	for _, tc := range []struct {
		method string
		safe   bool
	}{{"GET", true}, {"HEAD", true}, {"OPTIONS", true}, {"TRACE", true}, {"POST", false}, {"PUT", false}, {"DELETE", false}, {"PATCH", false}} {
		if got := retrySafeMethod(tc.method); got != tc.safe {
			t.Errorf("%s: got %v", tc.method, got)
		}
	}
}

func TestFallbackHTTPFailureDoesNotRetryPrimary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer fallback.Close()
	a := &App{cfg: Config{PrimaryAddr: strings.TrimPrefix(primary.URL, "http://"), FallbackAddr: strings.TrimPrefix(fallback.URL, "http://")}, mode: ModeFallback, log: log.New(io.Discard, "", 0), transports: map[string]*http.Transport{}}
	for _, body := range []io.Reader{strings.NewReader("payment"), io.LimitReader(strings.NewReader("stream"), 6)} {
		r := httptest.NewRequest(http.MethodPost, "http://example.test/pay", body)
		w := httptest.NewRecorder()
		a.handleHTTP(w, r)
		if w.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502", w.Code)
		}
	}
	if n := primaryCalls.Load(); n != 0 {
		t.Fatalf("fallback failure retried %d requests through primary", n)
	}
}

func TestDaemonIdentity(t *testing.T) {
	id := processIdentity(os.Getpid())
	if id == "" {
		t.Skip("process identity unavailable")
	}
	st := &State{PID: os.Getpid(), ProcessIdentity: id}
	if !daemonMatches(st) {
		t.Fatal("own process identity mismatch")
	}
	st.ProcessIdentity = "wrong"
	if daemonMatches(st) {
		t.Fatal("accepted wrong identity")
	}
}

func TestRestoreEmptyServerIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CALL_LOG"
case "$1" in
-getwebproxy)
  IFS= read -r enabled < "$MOCK_DIR/web"
  printf 'Enabled: %s\nServer: 127.0.0.1\nPort: 33333\nAuthenticated Proxy Enabled: 0\n' "$enabled";;
-getsecurewebproxy)
  IFS= read -r server < "$MOCK_DIR/secure"
  if [ "$server" = conflict ]; then printf 'Enabled: Yes\nServer: other.example\nPort: 9000\n'; else printf 'Enabled: No\nServer: \nPort: 0\n'; fi;;
-getsocksfirewallproxy) printf 'Enabled: No\nServer: \nPort: 0\n';;
-getproxybypassdomains) printf "There aren't any bypass domains\n";;
-getautoproxyurl) printf 'URL: (null)\nEnabled: No\n';;
-getproxyautodiscovery) printf 'Off\n';;
-setwebproxystate)
  if [ -f "$MOCK_DIR/fail" ]; then exit 1; fi
  printf 'No\n' > "$MOCK_DIR/web";;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "networksetup"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("web", "Yes")
	write("secure", "conflict")
	t.Setenv("PATH", dir)
	t.Setenv("MOCK_DIR", dir)
	t.Setenv("CALL_LOG", log)
	snapshot := &ServiceSnapshot{Name: "test"}
	if err := restoreOneProxy(snapshot, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil || !strings.Contains(err.Error(), "secure: changed externally") {
		t.Fatalf("expected secure conflict, got %v", err)
	}
	web, err := os.ReadFile(filepath.Join(dir, "web"))
	if err != nil || string(web) != "No\n" {
		t.Fatalf("web not disabled: %q, %v", web, err)
	}
	write("secure", "original")
	if err := restoreOneProxy(snapshot, "127.0.0.1:33333", "127.0.0.1:33334"); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if err := restoreOneProxy(snapshot, "127.0.0.1:33333", "127.0.0.1:33334"); err != nil {
		t.Fatalf("repeat failed: %v", err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "-setwebproxystate test off"); n != 1 {
		t.Fatalf("expected one disable, got %d: %s", n, calls)
	}
	write("web", "Yes")
	write("fail", "1")
	if err := restoreOneProxy(snapshot, "127.0.0.1:33333", "127.0.0.1:33334"); err == nil {
		t.Fatal("expected failed disable")
	}
	if err := os.Remove(filepath.Join(dir, "fail")); err != nil {
		t.Fatal(err)
	}
	if err := restoreOneProxy(snapshot, "127.0.0.1:33333", "127.0.0.1:33334"); err != nil {
		t.Fatalf("retry after failed disable: %v", err)
	}
}

func TestRestoreDoesNotOverwriteChangedProxy(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$CALL_LOG"
case "$1" in
-getwebproxy|-getsecurewebproxy|-getsocksfirewallproxy) printf 'Enabled: Yes\nServer: other.example\nPort: 1234\nAuthenticated Proxy Enabled: 0\n';;
-getproxybypassdomains) printf "There aren't any bypass domains\n";;
-getautoproxyurl) printf 'URL: (null)\nEnabled: No\n';;
-getproxyautodiscovery) printf 'Off\n';;
*) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "networksetup"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CALL_LOG", log)
	err := restoreOneProxy(&ServiceSnapshot{Name: "test"}, "127.0.0.1:33333", "127.0.0.1:33334")
	if err == nil {
		t.Fatal("expected conflict")
	}
	b, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "-set") {
		t.Fatalf("unexpected mutation: %s", b)
	}
}

func TestSOCKSListenRequiresLiteralLoopback(t *testing.T) {
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "localhost:0", "192.0.2.1:0", "invalid"} {
		a := &App{cfg: Config{SOCKSListen: addr}}
		if err := a.startSOCKSProxy(); err == nil {
			a.socksLn.Close()
			t.Errorf("accepted %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		a := &App{cfg: Config{SOCKSListen: addr}}
		if err := a.startSOCKSProxy(); err != nil {
			if addr == "[::1]:0" {
				t.Logf("IPv6 unavailable: %v", err)
				continue
			}
			t.Errorf("%s: %v", addr, err)
			continue
		}
		a.socksLn.Close()
	}
}

func TestSOCKSMethodNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		request, reply []byte
	}{
		{"no authentication", []byte{5, 1, 0}, []byte{5, 0}},
		{"among methods", []byte{5, 2, 2, 0}, []byte{5, 0}},
		{"unsupported", []byte{5, 1, 2}, []byte{5, 255}},
		{"empty", []byte{5, 0}, []byte{5, 255}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			done := make(chan struct{})
			go func() { (&App{}).handleSOCKS(server); close(done) }()
			client.SetDeadline(time.Now().Add(time.Second))
			if _, err := client.Write(tc.request); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, 2)
			if _, err := io.ReadFull(client, reply); err != nil || !bytes.Equal(reply, tc.reply) {
				t.Fatalf("reply %v: %v", reply, err)
			}
			client.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not close")
			}
		})
	}
}

func TestProxyAuthorityOverTCP(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
			received := make(chan *http.Request, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodConnect {
					c, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						return
					}
					defer c.Close()
					fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
					req, err := http.ReadRequest(rw.Reader)
					if err != nil {
						return
					}
					received <- req
					fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
					return
				}
				received <- r
				w.Header().Set("Connection", "close")
				fmt.Fprint(w, "ok")
			}))
			defer upstream.Close()
			a := &App{cfg: Config{FallbackAddr: strings.TrimPrefix(upstream.URL, "http://")}, mode: ModeFallback, blockedDomains: map[string]struct{}{"blocked.example": {}}, transports: map[string]*http.Transport{}}
			proxy := httptest.NewServer(a)
			defer proxy.Close()
			for _, host := range []string{"allowed.example", "blocked.example"} {
				c, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				c.SetDeadline(time.Now().Add(3 * time.Second))
				extra := ""
				if upgrade {
					extra = "Connection: Upgrade\r\nUpgrade: websocket\r\n"
				}
				fmt.Fprintf(c, "GET http://allowed.example/path HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n", host, extra)
				resp, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil {
					c.Close()
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				c.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("status %d", resp.StatusCode)
				}
				select {
				case req := <-received:
					if req.Host != "allowed.example" {
						t.Errorf("upstream Host = %q", req.Host)
					}
				case <-time.After(time.Second):
					t.Fatal("no upstream request")
				}
			}
		})
	}
}

func TestProxyNormalizesDirectHandlerHost(t *testing.T) {
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received <- r.Host; w.WriteHeader(204) }))
	defer upstream.Close()
	a := &App{cfg: Config{FallbackAddr: strings.TrimPrefix(upstream.URL, "http://")}, mode: ModeFallback, transports: map[string]*http.Transport{}}
	r := httptest.NewRequest("GET", "http://allowed.example/", nil)
	r.Host = "blocked.example"
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("status %d", w.Code)
	}
	if host := <-received; host != "allowed.example" {
		t.Fatalf("upstream Host %q", host)
	}
}

func TestBlockedProxyPathsOverTCP(t *testing.T) {
	a := &App{blockedDomains: map[string]struct{}{"blocked.example": {}}, mode: ModeProbing, probeEnd: time.Now().Add(time.Hour)}
	proxy := httptest.NewServer(a)
	defer proxy.Close()
	for _, request := range []string{
		"GET http://BLOCKED.example./ HTTP/1.1\r\nHost: allowed.example\r\nConnection: close\r\n\r\n",
		"GET http://blocked.example/socket HTTP/1.1\r\nHost: allowed.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
		"CONNECT blocked.example:443 HTTP/1.1\r\nHost: blocked.example:443\r\n\r\n",
	} {
		c, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(time.Second))
		fmt.Fprint(c, request)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		c.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("request %q: status %d", request, resp.StatusCode)
		}
	}
	// Exercise a mismatched Host directly as well: net/http uses the absolute URI's authority on the wire.
	for _, upgrade := range []bool{false, true} {
		r := httptest.NewRequest("GET", "http://blocked.example/", nil)
		r.Host = "allowed.example"
		if upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("upgrade=%v: status %d", upgrade, w.Code)
		}
	}
}

// tcpPair exposes actual TCP half-close semantics on both ends.
func statsTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return c.(*net.TCPConn), peer.(*net.TCPConn)
}
func TestRelayBufferedHalfClose(t *testing.T) {
	client, clientPeer := statsTCPPair(t)
	upstream, upstreamPeer := statsTCPPair(t)
	defer clientPeer.Close()
	defer upstreamPeer.Close()
	_ = clientPeer.SetDeadline(time.Now().Add(3 * time.Second))
	_ = upstreamPeer.SetDeadline(time.Now().Add(3 * time.Second))
	a := &App{}
	done := make(chan struct{})
	go func() {
		a.relay(&bufferedConn{Conn: client, r: bufio.NewReader(client)}, &bufferedConn{Conn: upstream, r: bufio.NewReader(upstream)}, "direct", "example.com")
		close(done)
	}()
	_, _ = clientPeer.Write([]byte("request"))
	_ = clientPeer.CloseWrite()
	req, err := io.ReadAll(upstreamPeer)
	if err != nil || string(req) != "request" {
		t.Fatalf("request %q %v", req, err)
	}
	_, _ = upstreamPeer.Write([]byte("response"))
	_ = upstreamPeer.CloseWrite()
	resp, err := io.ReadAll(clientPeer)
	if err != nil || string(resp) != "response" {
		t.Fatalf("response %q %v", resp, err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay stuck")
	}
}

type failReadConn struct{ net.Conn }

func (c failReadConn) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func TestRelayErrorUnblocksOppositeCopy(t *testing.T) {
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	defer clientPeer.Close()
	defer upstreamPeer.Close()
	a := &App{}
	done := make(chan struct{})
	go func() { a.relay(failReadConn{client}, upstream, "direct", "example.com"); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("error left relay blocked")
	}
}
func TestBufferedConnUnsupportedHalfCloseCloses(t *testing.T) {
	c, peer := net.Pipe()
	defer peer.Close()
	b := &bufferedConn{Conn: c, r: bufio.NewReader(c)}
	if err := b.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("got %v", err)
	}
}

type hijackBlockedWriter struct {
	conn     net.Conn
	hijacked chan struct{}
}

func (w *hijackBlockedWriter) Header() http.Header       { return make(http.Header) }
func (w *hijackBlockedWriter) Write([]byte) (int, error) { return 0, errors.New("unexpected") }
func (w *hijackBlockedWriter) WriteHeader(int)           {}
func (w *hijackBlockedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	close(w.hijacked)
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestShutdownUnblocksHijackedHandshake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(appSupportDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveRoutingMode("direct"); err != nil {
		t.Fatal(err)
	}
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprint(upgrade), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				c, e := ln.Accept()
				if e == nil {
					accepted <- c
				}
			}()
			client, peer := net.Pipe()
			defer peer.Close()
			w := &hijackBlockedWriter{client, make(chan struct{})}
			a := &App{cfg: defaultConfig(), log: log.New(io.Discard, "", 0)}
			if !a.beginProxyWork() {
				t.Fatal("admission")
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer a.proxyWork.Done()
				if upgrade {
					r := httptest.NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
					a.handleUpgrade(w, r)
				} else {
					r := httptest.NewRequest("CONNECT", "http://"+ln.Addr().String(), nil)
					r.Host = ln.Addr().String()
					a.handleConnect(w, r)
				}
			}()
			server := <-accepted
			defer server.Close()
			<-w.hijacked
			stopped := make(chan struct{})
			go func() { a.stopProxyWork(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown blocked on hijacked write")
			}
			<-done
		})
	}
}
