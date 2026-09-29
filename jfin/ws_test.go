package jfin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func discardLog() *log.Logger { return log.New(io.Discard, "", 0) }

func TestWSConnectKeepaliveDedupe(t *testing.T) {
	var capsCalled atomic.Bool
	var dials atomic.Int32
	var keepAlives atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Sessions/Capabilities/Full":
			capsCalled.Store(true)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case "/Sessions":
			_, _ = w.Write([]byte(`[{"DeviceId":"dev-1"}]`))
		case "/socket":
			dials.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			ctx := r.Context()
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"ForceKeepAlive","Data":{"TimeoutInterval":100},"MessageId":"fk1"}`))
			m := `{"MessageType":"Ping","Data":{"n":1},"MessageId":"m1"}`
			_ = conn.Write(ctx, websocket.MessageText, []byte(m))
			_ = conn.Write(ctx, websocket.MessageText, []byte(m)) // duplicate MessageId
			for {
				_, b, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if string(b) == `{"MessageType":"KeepAlive"}` {
					keepAlives.Add(1)
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "d", "dev-1", "1", false)
	ws := NewWS(c, discardLog())
	ws.HealthInterval = time.Hour // disabled for this test
	var pings atomic.Int32
	ws.On("Ping", func(_ context.Context, _ json.RawMessage) { pings.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = ws.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for !capsCalled.Load() || pings.Load() != 1 || keepAlives.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("caps=%v pings=%d keepAlives=%d", capsCalled.Load(), pings.Load(), keepAlives.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // give the duplicate a chance to leak through
	if pings.Load() != 1 {
		t.Errorf("dedupe failed, pings=%d, want 1", pings.Load())
	}
	cancel()
	<-done
	if dials.Load() != 1 {
		t.Errorf("dials=%d, want 1", dials.Load())
	}
}

func TestWSReconnect(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			dials.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				conn.Close(websocket.StatusGoingAway, "")
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New(srv.URL, "d", "dev-1", "1", false)
	ws := NewWS(c, discardLog())
	ws.HealthInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	go func() { _ = ws.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for dials.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("dials=%d, want >=2 (reconnect with backoff)", dials.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWSHealthCheckForcesReconnect(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Sessions":
			_, _ = w.Write([]byte(`[{"DeviceId":"some-other-device"}]`))
		case "/socket":
			dials.Add(1)
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			for {
				if _, _, err := conn.Read(r.Context()); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "d", "dev-1", "1", false)
	ws := NewWS(c, discardLog())
	ws.HealthInterval = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() { _ = ws.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for dials.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("dials=%d, want >=2 (health-check reconnect)", dials.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The server sends UserDataChanged/RefreshProgress/… on every web-UI action.
// They must be ignored quietly: one line per type, then a count every 50.
func TestWSIgnoresUnactedMessagesQuietly(t *testing.T) {
	var logBuf bytes.Buffer
	lg := log.New(&logBuf, "", 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Sessions/Capabilities/Full", "/Sessions":
			_, _ = w.Write([]byte(`[]`))
		case "/socket":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			ctx := r.Context()
			for i := 0; i < 3; i++ {
				_ = conn.Write(ctx, websocket.MessageText,
					[]byte(`{"MessageType":"UserDataChanged","Data":{"EnableUserData":true}}`))
			}
			for i := 0; i < 120; i++ {
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`))
			}
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "d", "dev-1", "1", false)
	ws := NewWS(c, lg)
	ws.HealthInterval = time.Hour // disabled for this test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = ws.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for ws.ignoredCount("KeepAlive") < 120 {
		if time.Now().After(deadline) {
			t.Fatalf("keepalives not received: %d\nlog:\n%s", ws.ignoredCount("KeepAlive"), logBuf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	out := logBuf.String()
	if n := strings.Count(out, "ignoring UserDataChanged"); n != 1 {
		t.Errorf("UserDataChanged logged %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "unhandled") {
		t.Errorf("unhandled message type still logged:\n%s", out)
	}
	// 120 keepalives → 1 first-time line + summaries at 50 and 100.
	if got := strings.Count(out, "ignored 50 KeepAlive") + strings.Count(out, "ignored 100 KeepAlive"); got != 2 {
		t.Errorf("keepalive summaries = %d, want 2:\n%s", got, out)
	}
}

// The tray's status dot needs a three-way state: offline (not trying),
// reconnecting (dialing/backing off) and connected.
func TestWSConnectionState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Sessions/Capabilities/Full", "/Sessions":
			_, _ = w.Write([]byte(`[]`))
		case "/socket":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "d", "dev-1", "1", false)
	ws := NewWS(c, discardLog())
	ws.HealthInterval = time.Hour
	if got := ws.State(); got != StateOffline {
		t.Errorf("initial state = %d, want StateOffline", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = ws.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for ws.State() != StateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("never reached StateConnected (state=%d)", ws.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ws.Connected() {
		t.Error("StateConnected but Connected() is false")
	}
	cancel()
	<-done
	if got := ws.State(); got != StateOffline {
		t.Errorf("state after shutdown = %d, want StateOffline", got)
	}
	if ws.Connected() {
		t.Error("Connected() still true after shutdown")
	}
}
