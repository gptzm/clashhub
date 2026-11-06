package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type errorBody struct{ err error }

func (b errorBody) Read([]byte) (int, error) { return 0, b.err }
func (errorBody) Close() error               { return nil }

type failingResponseWriter struct{ header http.Header }

func (w *failingResponseWriter) Header() http.Header     { return w.header }
func (*failingResponseWriter) WriteHeader(int)           {}
func (*failingResponseWriter) Write([]byte) (int, error) { return 0, errors.New("client write failed") }

func runResponseAndCaptureAbort(fn func()) (abort any) {
	defer func() { abort = recover() }()
	fn()
	return nil
}

func TestUpstreamBodyErrorSignalsOnlyValidReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		err                           error
		direct, canceled, stale, want bool
	}{
		{"reset", errors.New("upstream reset"), false, false, false, true},
		{"eof", io.EOF, false, false, false, false},
		{"closed", net.ErrClosed, false, false, false, false},
		{"cancelled", errors.New("upstream reset"), false, true, false, false},
		{"direct", errors.New("upstream reset"), true, false, false, false},
		{"stale", errors.New("upstream reset"), false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveRoutingMode("rule"); err != nil {
				t.Fatal(err)
			}
			a := &App{nodeScanWake: make(chan struct{}, 1)}
			token, ok := a.snapshotProbeToken()
			if !ok {
				t.Fatal("route unavailable")
			}
			if tc.stale {
				a.engineMu.Lock()
				a.selectionGeneration++
				a.engineMu.Unlock()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			upstream := "127.0.0.1:33301"
			if tc.direct {
				upstream = "direct"
			}
			abort := runResponseAndCaptureAbort(func() {
				a.writeResponse(httptest.NewRecorder(), &http.Response{StatusCode: 200, Header: make(http.Header), Body: errorBody{tc.err}}, "example.com", upstream, ctx, token)
			})
			if (tc.err != io.EOF && abort != http.ErrAbortHandler) || (tc.err == io.EOF && abort != nil) {
				t.Fatalf("unexpected abort %v", abort)
			}
			select {
			case <-a.nodeScanWake:
				if !tc.want {
					t.Fatal("unexpected scan")
				}
			default:
				if tc.want {
					t.Fatal("missing scan")
				}
			}
		})
	}
}

func TestTruncatedResponseAbortsHTTPStream(t *testing.T) {
	a := &App{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.writeResponse(w, &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": []string{"8"}}, Body: io.NopCloser(&chunkAndErrorReader{})}, "example.test", "direct", r.Context(), probeToken{})
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		return
	} // An abort may arrive before headers are flushed.
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("truncated body was reported complete")
	}
}

func TestClientWriteFailureDoesNotSignalUpstream(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &App{nodeScanWake: make(chan struct{}, 1)}
	token, _ := a.snapshotProbeToken()
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&oneChunkReader{})}
	if abort := runResponseAndCaptureAbort(func() {
		a.writeResponse(&failingResponseWriter{header: make(http.Header)}, resp, "example.com", "127.0.0.1:33301", context.Background(), token)
	}); abort != http.ErrAbortHandler {
		t.Fatalf("expected abort, got %v", abort)
	}
	select {
	case <-a.nodeScanWake:
		t.Fatal("client write failure triggered scan")
	default:
	}
	// If a read returned bytes and an error, io.Copy gives the client write
	// failure precedence; that outcome is not unambiguously attributable.
	resp = &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&chunkAndErrorReader{})}
	if abort := runResponseAndCaptureAbort(func() {
		a.writeResponse(&failingResponseWriter{header: make(http.Header)}, resp, "example.com", "127.0.0.1:33301", context.Background(), token)
	}); abort != http.ErrAbortHandler {
		t.Fatalf("expected abort, got %v", abort)
	}
	select {
	case <-a.nodeScanWake:
		t.Fatal("ambiguous client write failure triggered scan")
	default:
	}
}

type oneChunkReader struct{}

func (*oneChunkReader) Read(p []byte) (int, error) { copy(p, "data"); return 4, nil }

type chunkAndErrorReader struct{}

func (*chunkAndErrorReader) Read(p []byte) (int, error) {
	copy(p, "data")
	return 4, errors.New("upstream read failed")
}
