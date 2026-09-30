package player

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net"
	"testing"
	"time"
)

// mpv puts an event's fields at the top level of the event object, not inside
// "data": end-file carries reason and playlist_entry_id there, and the player
// needs both (a reason-less end-file looks like a "stop", so the queue never
// advances, and the entry id is what identifies the file a replace dropped).
func TestHookSeesTopLevelEventFields(t *testing.T) {
	shim, mpv := net.Pipe() // we write on one end, the read loop reads the other
	defer shim.Close()
	p := &Proc{log: log.New(io.Discard, "", 0), pending: map[int64]chan *rpcMsg{}}
	got := make(chan json.RawMessage, 1)
	p.hook = func(name string, data json.RawMessage) {
		if name == "end-file" {
			got <- data
		}
	}
	p.conn, p.r = mpv, bufio.NewReader(mpv)
	go p.readLoop()
	shim.Write([]byte(`{"event":"end-file","reason":"eof","playlist_entry_id":7}` + "\n")) //nolint:errcheck

	var e struct {
		Reason string `json:"reason"`
		Entry  int64  `json:"playlist_entry_id"`
	}
	select {
	case data := <-got:
		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatalf("payload %s: %v", data, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no end-file reached the hook")
	}
	if e.Reason != "eof" || e.Entry != 7 {
		t.Errorf("end-file = %+v, want reason eof entry 7", e)
	}
}
