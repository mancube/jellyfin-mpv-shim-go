package jfin

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// WSMessage is the /socket envelope (server → client).
type WSMessage struct {
	MessageType string          `json:"MessageType"`
	Data        json.RawMessage `json:"Data,omitempty"`
	MessageID   string          `json:"MessageId,omitempty"`
}

// WSHandler receives the Data payload of one server message type.
type WSHandler func(ctx context.Context, data json.RawMessage)

// WS maintains the /socket connection: keepalive, MessageId dedupe,
// exponential-backoff reconnect (1s → 100s cap, reset on successful
// connect), and a periodic /Sessions health check.
type WS struct {
	c        *Client
	log      *log.Logger
	handlers map[string]WSHandler
	seen     map[string]struct{}
	ignMu    sync.Mutex     // guards ignored
	ignored  map[string]int // per-type count of messages we do not act on
	live     atomic.Bool    // true while a socket is up (for the TUI/tray)
	state    atomic.Int32   // StateOffline / StateReconnecting / StateConnected

	// HealthInterval is the /Sessions poll period (0 disables).
	HealthInterval time.Duration
	// RetryMins stops the reconnect loop after this many minutes without a
	// connection (0 = retry forever). Upstream connect_retry_mins.
	RetryMins int
}

// NewWS builds a WS client for c.
func NewWS(c *Client, logger *log.Logger) *WS {
	return &WS{
		c:              c,
		log:            logger,
		handlers:       map[string]WSHandler{},
		seen:           map[string]struct{}{},
		ignored:        map[string]int{},
		HealthInterval: 300 * time.Second,
	}
}

// On registers a handler for a server message type (e.g. "Play").
// Must be called before Run.
func (w *WS) On(msgType string, h WSHandler) {
	w.handlers[msgType] = h
}

// Connection states, for the TUI and the tray's status dot.
const (
	StateOffline      int32 = 0 // not connected, not trying (before start / after stop)
	StateReconnecting int32 = 1 // dialing or waiting out the backoff
	StateConnected    int32 = 2 // socket is up
)

// Connected reports whether the socket is currently up.
func (w *WS) Connected() bool { return w.live.Load() }

// State is the connection state (StateOffline / StateReconnecting /
// StateConnected). The tray renders it as the status dot.
func (w *WS) State() int32 { return w.state.Load() }

// Run blocks until ctx is canceled, reconnecting forever.
func (w *WS) Run(ctx context.Context) error {
	backoff := time.Second
	var firstAttempt time.Time
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.state.Store(StateReconnecting)
		if firstAttempt.IsZero() {
			firstAttempt = time.Now()
		}
		ok, err := w.connect(ctx)
		if ctx.Err() != nil {
			w.state.Store(StateOffline)
			return ctx.Err()
		}
		if !ok {
			w.log.Printf("ws: %v; retry in %s", err, backoff)
			backoff = min(backoff*2, 100*time.Second)
		} else {
			backoff = time.Second // reset only after a successful connect
			firstAttempt = time.Time{}
		}
		if w.RetryMins > 0 && !firstAttempt.IsZero() &&
			time.Since(firstAttempt) > time.Duration(w.RetryMins)*time.Minute {
			w.log.Printf("ws: no connection after %d minutes; giving up", w.RetryMins)
			w.state.Store(StateOffline)
			return err
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			w.state.Store(StateOffline)
			return ctx.Err()
		case <-t.C:
		}
	}
}

// connect dials, registers capabilities, and runs the read loop.
// ok=true means the connection was established (even if the server then
// closed it, e.g. a server restart).
func (w *WS) connect(ctx context.Context) (ok bool, err error) {
	url := w.wsURL()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: w.c.http,
		HTTPHeader: http.Header{
			"Authorization": []string{w.c.AuthHeader()},
		},
	})
	if err != nil {
		return false, err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	w.log.Printf("ws: connected to %s", url)
	w.live.Store(true)
	w.state.Store(StateConnected)
	defer func() {
		w.live.Store(false)
		w.state.Store(StateOffline)
	}()
	if err := w.c.PostCapabilities(ctx); err != nil {
		w.log.Printf("ws: capabilities: %v", err)
	}
	w.readLoop(ctx, conn)
	return true, nil
}

func (w *WS) readLoop(ctx context.Context, conn *websocket.Conn) {
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	done := make(chan struct{})
	defer close(done)

	// keepalive goroutine, (re)started by ForceKeepAlive
	var ka *time.Ticker
	var kaDone chan struct{}
	stopKeepalive := func() {
		if kaDone != nil {
			close(kaDone)
		}
	}
	defer stopKeepalive()

	if w.HealthInterval > 0 {
		t := time.NewTicker(w.HealthInterval)
		go func() {
			for {
				select {
				case <-t.C:
					if !w.c.Healthy(ctx) {
						w.log.Printf("ws: health check failed, reconnecting")
						cancelRead()
					}
				case <-done:
					t.Stop()
					return
				}
			}
		}()
	}

	for {
		_, b, err := conn.Read(readCtx)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Printf("ws: read: %v", err)
			}
			return
		}
		var msg WSMessage
		if err := json.Unmarshal(b, &msg); err != nil {
			w.log.Printf("ws: bad message: %v", err)
			continue
		}
		if msg.MessageID != "" {
			if _, dup := w.seen[msg.MessageID]; dup {
				continue
			}
			w.seen[msg.MessageID] = struct{}{}
			if len(w.seen) > 4096 { // bound memory; ids only matter briefly
				w.seen = map[string]struct{}{}
			}
		}
		switch msg.MessageType {
		case "ForceKeepalive", "ForceKeepAlive":
			var d struct {
				TimeoutInterval int `json:"TimeoutInterval"`
			}
			_ = json.Unmarshal(msg.Data, &d)
			interval := time.Duration(d.TimeoutInterval) * time.Millisecond
			if interval <= 0 {
				interval = 15 * time.Second
			}
			stopKeepalive()
			ka, kaDone = time.NewTicker(interval), make(chan struct{})
			go func(ka *time.Ticker, doneC chan struct{}) {
				for {
					select {
					case <-ka.C:
						if err := conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`)); err != nil {
							return
						}
					case <-doneC:
						ka.Stop()
						return
					}
				}
			}(ka, kaDone)
		case "KeepAlive":
			// Server heartbeat broadcast; the client→server KeepAlive we send
			// (driven by ForceKeepAlive) is what the server monitors. Same as
			// apiclient: ignore.
			w.ignore("KeepAlive")
		default:
			if h := w.handlers[msg.MessageType]; h != nil {
				h(ctx, msg.Data)
			} else {
				// UserDataChanged, RefreshProgress, SessionUpdate… the server
				// sends these on every web-UI interaction; upstream only
				// debug-logs them. Log the first of each type, then a count
				// every 50, so the log (and the TUI) stay readable.
				w.ignore(msg.MessageType)
			}
		}
	}
}

// ignoredCount is a test/debug accessor for the ignore counters.
func (w *WS) ignoredCount(msgType string) int {
	w.ignMu.Lock()
	defer w.ignMu.Unlock()
	return w.ignored[msgType]
}

// ignore records a message we deliberately do not act on, logging the first
// occurrence of each type and then every 50th.
func (w *WS) ignore(msgType string) {
	w.ignMu.Lock()
	n := w.ignored[msgType] + 1
	w.ignored[msgType] = n
	w.ignMu.Unlock()
	switch {
	case n == 1:
		w.log.Printf("ws: ignoring %s messages", msgType)
	case n%50 == 0:
		w.log.Printf("ws: ignored %d %s messages", n, msgType)
	}
}

func (w *WS) wsURL() string {
	base := w.c.Base
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + "/socket"
	default:
		return "ws://" + strings.TrimPrefix(base, "http://") + "/socket"
	}
}
