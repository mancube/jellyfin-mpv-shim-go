package jfin

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
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
