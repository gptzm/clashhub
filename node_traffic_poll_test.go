package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNodeTrafficPollRealHTTP(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxies":
			fmt.Fprint(w, `{"proxies":{"actual":{"type":"Vless"},"selected":{"type":"Selector","all":["actual"]}}}`)
		case "/connections":
			count++
			fmt.Fprintf(w, `{"uploadTotal":%d,"downloadTotal":%d,"connections":[{"id":"x","start":%q,"upload":%d,"download":%d,"chains":["actual","selected"]}]}`, 100+count*10, 200+count*20, now, count*10, count*20)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := &App{}
	p := &ownedEngine{cmd: &exec.Cmd{}, done: make(chan struct{})}
	a.setOwnedEngineLocked(p)
	defer a.setOwnedEngineLocked(nil)
	s, _ := openNodeTraffic(filepath.Join(t.TempDir(), "traffic"), t.Logf)
	s.client = srv.Client()
	if err := s.poll(context.Background(), a, srv.URL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := s.poll(context.Background(), a, srv.URL); err != nil {
		t.Fatal(err)
	}
	got := s.query(60, time.Now())
	if len(got.Nodes) != 1 || got.Nodes[0].Node != "actual" || got.Nodes[0].Upload != 10 || got.Nodes[0].Download != 20 || !got.Nodes[0].RateAvailable {
		t.Fatalf("%+v", got)
	}
	close(p.done)
	if err := s.poll(context.Background(), a, srv.URL); err == nil {
		t.Fatal("stopped engine accepted")
	}
}
func TestNodeTrafficReadSecurity(t *testing.T) {
	s := &statsIntegration{nodeTraffic: &nodeTrafficService{}}
	for _, h := range []string{"Origin", "Sec-Fetch-Site"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:33335/api/node-traffic", nil)
		r.RemoteAddr = "127.0.0.1:42"
		r.URL.Scheme = ""
		r.URL.Host = ""
		r.Header.Set(h, "same-site")
		w := httptest.NewRecorder()
		s.serveHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(h, w.Code)
		}
	}
	if _, err := trafficProxyNames(map[string]proxyInfo{strings.Repeat("x", 513): {Type: "Vless"}}); err != nil {
		t.Fatal(err)
	}
}
