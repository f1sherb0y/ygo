package server_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/server"
)

// hpConn is a raw Hocuspocus client: every frame it writes and reads starts
// with the document name.
type hpConn struct {
	t    *testing.T
	ws   *websocket.Conn
	name string
}

func hpDial(t *testing.T, url, name string) *hpConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &hpConn{t, ws, name}
}

func (c *hpConn) send(envelope []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute(c.name), envelope...)); err != nil {
		c.t.Fatal(err)
	}
}

// read returns the next frame, or the close status that ended the
// connection.
func (c *hpConn) read() (*syncpkg.Frame, websocket.StatusCode) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, raw, err := c.ws.Read(ctx)
	if err != nil {
		if status := websocket.CloseStatus(err); status != -1 {
			return nil, status
		}
		c.t.Fatalf("read: %v", err)
	}
	route, rest, err := syncpkg.DecodeRoute(raw)
	if err != nil || route != c.name {
		c.t.Fatalf("frame for %q, %v; want %q", route, err, c.name)
	}
	frame, _, err := syncpkg.DecodeEnvelope(rest)
	if err != nil {
		c.t.Fatal(err)
	}
	return frame, 0
}

// until reads frames until one satisfies want.
func (c *hpConn) until(want func(*syncpkg.Frame) bool) *syncpkg.Frame {
	c.t.Helper()
	for range 20 {
		frame, status := c.read()
		if frame == nil {
			c.t.Fatalf("connection closed with %d", status)
		}
		if want(frame) {
			return frame
		}
	}
	c.t.Fatal("the frame never came")
	return nil
}

func update(t *testing.T, key, value string) []byte {
	t.Helper()
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	m.Set(txn, key, value)
	txn.Commit()
	return ygo.EncodeStateAsUpdate(d)
}

func isType(want syncpkg.MessageType) func(*syncpkg.Frame) bool {
	return func(f *syncpkg.Frame) bool { return f.Type == want }
}

func TestHocuspocus_DocumentComesFromTheFrames(t *testing.T) {
	var connected []string
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnConnect: func(_, docName string, _ *http.Request) error {
			connected = append(connected, docName)
			return nil
		},
	})
	// The URL path is not the document: both connect to the same URL.
	a, b, other := hpDial(t, url+"/ignored", "room"), hpDial(t, url, "room"), hpDial(t, url, "elsewhere")
	for _, c := range []*hpConn{a, b, other} {
		c.send(syncpkg.EncodeSyncStep1([]byte{0}))
		c.until(func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2 })
	}
	if strings.Join(connected, ",") != "room,room,elsewhere" {
		t.Errorf("OnConnect saw %v", connected)
	}

	a.send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	// The sender gets its acknowledgement, the room the update.
	ack := a.until(isType(syncpkg.MessageSyncStatus))
	if len(ack.Payload) != 1 || ack.Payload[0] != 1 {
		t.Errorf("acknowledgement payload %v, want [1]", ack.Payload)
	}
	got := b.until(func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate })
	d := ygo.NewDoc()
	if err := ygo.ApplyUpdate(d, got.Payload); err != nil || ygo.NewMap(d, "m").Get("k") != "v" {
		t.Errorf("the room got %v, %v", ygo.NewMap(d, "m").Get("k"), err)
	}
	// The other document hears nothing of it: its next frame is the
	// answer to its own question.
	other.send(syncpkg.EncodeQueryAwareness())
	other.send(syncpkg.EncodeSyncStep1([]byte{0}))
	if f := other.until(isType(syncpkg.MessageSync)); f.SyncSub != syncpkg.SyncStep2 {
		t.Errorf("the other document got sync sub-type %d", f.SyncSub)
	}
}

func TestHocuspocus_OneDocumentPerConnection(t *testing.T) {
	url, _ := startTestServer(t, server.Options{Hocuspocus: true, OriginPatterns: []string{"*"}})
	c := hpDial(t, url, "one")
	c.send(syncpkg.EncodeSyncStep1([]byte{0}))
	c.until(isType(syncpkg.MessageSync))
	c.name = "two"
	c.send(syncpkg.EncodeSyncStep1([]byte{0}))
	c.name = "one"
	for {
		frame, status := c.read()
		if frame != nil {
			continue
		}
		if status != websocket.StatusPolicyViolation {
			t.Errorf("closed with %d, want %d", status, websocket.StatusPolicyViolation)
		}
		return
	}
}

func TestHocuspocus_NothingBeforeTheToken(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnAuthenticate: func(docName, token string) error {
			if docName != "secret" || token != "let me in" {
				return errors.New("no")
			}
			return nil
		},
	})
	// Seed the document through an authenticated connection.
	seed := hpDial(t, url, "secret")
	seed.send(syncpkg.EncodeAuthToken("let me in"))
	if f := seed.until(isType(syncpkg.MessageAuth)); f.AuthSub != syncpkg.AuthAuthenticated || string(f.Payload) != syncpkg.ScopeReadWrite {
		t.Fatalf("auth reply %d %q", f.AuthSub, f.Payload)
	}
	// Only now does the server open the sync.
	if f := seed.until(isType(syncpkg.MessageSync)); f.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("after the token came sync sub-type %d, want SyncStep1", f.SyncSub)
	}
	seed.send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	seed.until(isType(syncpkg.MessageSyncStatus))

	// Asking for the document without a token gets the connection closed,
	// and not one frame before that.
	nosy := hpDial(t, url, "secret")
	nosy.send(syncpkg.EncodeSyncStep1([]byte{0}))
	if frame, status := nosy.read(); frame != nil || status != syncpkg.CloseStatusUnauthorized {
		t.Errorf("without a token: frame %+v, close %d; want close %d", frame, status, syncpkg.CloseStatusUnauthorized)
	}

	// A refused token is told why, then closed.
	wrong := hpDial(t, url, "secret")
	wrong.send(syncpkg.EncodeAuthToken("guess"))
	if f, _ := wrong.read(); f == nil || f.Type != syncpkg.MessageAuth || f.AuthSub != syncpkg.AuthPermissionDenied || string(f.Payload) != "no" {
		t.Errorf("refused token got %+v", f)
	}
	for {
		frame, status := wrong.read()
		if frame != nil {
			if frame.Type == syncpkg.MessageSync || frame.Type == syncpkg.MessageAwareness {
				t.Errorf("a refused connection got a frame of type %d", frame.Type)
			}
			continue
		}
		if status != syncpkg.CloseStatusUnauthorized {
			t.Errorf("closed with %d, want %d", status, syncpkg.CloseStatusUnauthorized)
		}
		break
	}
}

func TestHocuspocus_ReadOnlyUpdatesAreRefusedAloud(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		ReadOnly:       func(docName string, _ *http.Request) bool { return docName == "frozen" },
	})
	c := hpDial(t, url, "frozen")
	c.send(syncpkg.EncodeAuthToken(""))
	if f := c.until(isType(syncpkg.MessageAuth)); string(f.Payload) != syncpkg.ScopeReadOnly {
		t.Errorf("scope %q, want %q", f.Payload, syncpkg.ScopeReadOnly)
	}
	c.send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	if ack := c.until(isType(syncpkg.MessageSyncStatus)); len(ack.Payload) != 1 || ack.Payload[0] != 0 {
		t.Errorf("acknowledgement payload %v, want [0]", ack.Payload)
	}
	c.send(syncpkg.EncodeSyncStep1([]byte{0}))
	step2 := c.until(func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2 })
	d := ygo.NewDoc()
	if err := ygo.ApplyUpdate(d, step2.Payload); err != nil || ygo.NewMap(d, "m").Len() != 0 {
		t.Errorf("the read-only document took the update (%v)", err)
	}
}

func TestHocuspocus_FirstFrameMustNameADocument(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:       true,
		OriginPatterns:   []string{"*"},
		HandshakeTimeout: 150 * time.Millisecond,
	})
	// A y-websocket client's first frame starts with a message type, which
	// reads as an empty document name.
	plain := hpDial(t, url+"/room", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := plain.ws.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep1([]byte{0})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plain.ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Errorf("a frame without a document: %v, want close %d", err, websocket.StatusPolicyViolation)
	}
	// A client that says nothing is not kept.
	silent := hpDial(t, url, "room")
	start := time.Now()
	if _, _, err := silent.ws.Read(ctx); err == nil {
		t.Error("a silent client got a frame")
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("a silent client was kept for %v", waited)
	}
}

// A connection that has named its document but not shown a token is in the
// room's list already. What the room sends must still not reach it.
func TestHocuspocus_NoFanOutBeforeTheToken(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnAuthenticate: func(_, token string) error {
			if token != "let me in" {
				return errors.New("no")
			}
			return nil
		},
	})
	// An Auth message of a kind the server does not know is ignored: the
	// connection stays, unauthenticated.
	lurker := hpDial(t, url, "secret")
	lurker.send([]byte{byte(syncpkg.MessageAuth), 9})

	member := hpDial(t, url, "secret")
	member.send(syncpkg.EncodeAuthToken("let me in"))
	member.until(isType(syncpkg.MessageSync))
	member.send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	member.until(isType(syncpkg.MessageSyncStatus))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, raw, err := lurker.ws.Read(ctx); err == nil {
		t.Errorf("a connection without a token was sent % x", raw)
	}
}
