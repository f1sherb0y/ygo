package server_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
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

// One connection carries several documents, as providers that share a
// websocket use it: each is a session of its own.
func TestHocuspocus_SeveralDocumentsOnOneConnection(t *testing.T) {
	var mu sync.Mutex
	joined, left := map[string]string{}, []string{}
	url, srv := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnConnect: func(connID, docName string, _ *http.Request) error {
			mu.Lock()
			joined[docName] = connID
			mu.Unlock()
			return nil
		},
		OnDisconnect: func(_, docName string) {
			mu.Lock()
			left = append(left, docName)
			mu.Unlock()
		},
		OnAuthorize: func(_, docName, token string) (server.Grant, error) {
			if token != "key to "+docName {
				return server.Grant{}, errors.New("wrong key")
			}
			return server.Grant{ReadOnly: docName == "two"}, nil
		},
	})
	shared := hpDial(t, url, "one")
	// as speaks for one document of the shared connection.
	as := func(name string) *hpConn { return &hpConn{t, shared.ws, name} }
	// next reads the next frame whatever document it is for.
	next := func() (string, *syncpkg.Frame) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, raw, err := shared.ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		route, rest, err := syncpkg.DecodeRoute(raw)
		if err != nil {
			t.Fatal(err)
		}
		frame, _, err := syncpkg.DecodeEnvelope(rest)
		if err != nil {
			t.Fatal(err)
		}
		return route, frame
	}
	until := func(name string, want func(*syncpkg.Frame) bool) *syncpkg.Frame {
		t.Helper()
		for range 40 {
			if route, frame := next(); route == name && want(frame) {
				return frame
			}
		}
		t.Fatalf("the frame for %q never came", name)
		return nil
	}

	as("one").send(syncpkg.EncodeAuthToken("key to one"))
	as("two").send(syncpkg.EncodeAuthToken("key to two"))
	if f := until("one", isType(syncpkg.MessageAuth)); string(f.Payload) != syncpkg.ScopeReadWrite {
		t.Errorf("document one: scope %q", f.Payload)
	}
	if f := until("two", isType(syncpkg.MessageAuth)); string(f.Payload) != syncpkg.ScopeReadOnly {
		t.Errorf("document two: scope %q", f.Payload)
	}
	// Each document has a connection ID of its own.
	mu.Lock()
	one, two := joined["one"], joined["two"]
	mu.Unlock()
	if one == "" || two == "" || one == two {
		t.Fatalf("connection IDs %q and %q", one, two)
	}

	// What is written to one document is acknowledged for it, and reaches
	// its room and not the other's.
	watcher := hpDial(t, url, "one")
	watcher.send(syncpkg.EncodeAuthToken("key to one"))
	watcher.until(isType(syncpkg.MessageSync))
	as("one").send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	if ack := until("one", isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 1 {
		t.Error("document one refused the update")
	}
	watcher.until(func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate })
	as("two").send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	if ack := until("two", isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 0 {
		t.Error("document two, read-only, took the update")
	}
	// The server speaks to one of them.
	if !srv.SendStateless(two, "for two") {
		t.Fatal("document two's session was not found")
	}
	if f := until("two", isType(syncpkg.MessageStateless)); string(f.Payload) != "for two" {
		t.Errorf("stateless %q", f.Payload)
	}

	// A third document with a wrong token is turned away, and the
	// connection goes on with the two it has.
	as("three").send(syncpkg.EncodeAuthToken("guess"))
	if f := until("three", isType(syncpkg.MessageAuth)); f.AuthSub != syncpkg.AuthPermissionDenied {
		t.Errorf("document three: auth sub-type %d, want permission denied", f.AuthSub)
	}
	as("one").send(syncpkg.EncodeSyncUpdate(update(t, "k2", "v2")))
	if ack := until("one", isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 1 {
		t.Error("document one stopped working after another document was refused")
	}
	// It may try again with the right one.
	as("three").send(syncpkg.EncodeAuthToken("key to three"))
	if f := until("three", isType(syncpkg.MessageAuth)); f.AuthSub != syncpkg.AuthAuthenticated {
		t.Errorf("document three, second try: auth sub-type %d", f.AuthSub)
	}

	// When the connection ends, every document on it has left.
	shared.ws.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		gone := strings.Join(left, ",")
		mu.Unlock()
		// "three" left twice: refused once, then with the connection.
		if strings.Count(gone, "one") == 1 && strings.Count(gone, "two") == 1 && strings.Count(gone, "three") == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("left: %s", gone)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := srv.Connections("two"); len(got) != 0 {
		t.Errorf("document two still has %v", got)
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

// The token decides what a connection may do, the server answers one
// connection and tells all of them, and ends a connection whose access is
// gone: what an application with share links needs of a Hocuspocus server.
func TestHocuspocus_SessionsByToken(t *testing.T) {
	var srv *server.Server
	ids := make(chan string, 4)
	authors := make(chan string, 4)
	url, srv := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnAuthorize: func(connID, docName, token string) (server.Grant, error) {
			switch token {
			case "editor":
				ids <- connID
				return server.Grant{}, nil
			case "viewer":
				ids <- connID
				return server.Grant{ReadOnly: true}, nil
			}
			return server.Grant{}, errors.New("no")
		},
		OnSessionOpen: func(connID, docName string) {
			// From here the connection can be spoken to.
			if !srv.SendStateless(connID, "welcome to "+docName) {
				t.Errorf("no welcome to %s", connID)
			}
		},
		OnChangeFrom: func(connID, docName string, _ []byte) { authors <- connID },
		OnStatelessFrom: func(connID, docName, payload string) {
			if !srv.SendStateless(connID, "you said "+payload) {
				t.Errorf("no answer to %s", connID)
			}
			srv.BroadcastStateless(docName, "someone said "+payload)
		},
	})
	open := func(token, scope string) (*hpConn, string) {
		c := hpDial(t, url, "doc")
		c.send(syncpkg.EncodeAuthToken(token))
		if f := c.until(isType(syncpkg.MessageAuth)); f.AuthSub != syncpkg.AuthAuthenticated || string(f.Payload) != scope {
			t.Fatalf("token %q: auth reply %d %q, want scope %q", token, f.AuthSub, f.Payload, scope)
		}
		// The session is open: the welcome OnSessionOpen sends arrives.
		c.until(func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageStateless && string(f.Payload) == "welcome to doc"
		})
		return c, <-ids
	}
	editor, editorID := open("editor", syncpkg.ScopeReadWrite)
	viewer, viewerID := open("viewer", syncpkg.ScopeReadOnly)

	// The same document, two tokens: one writes, one is refused.
	viewer.send(syncpkg.EncodeSyncUpdate(update(t, "k", "viewer")))
	if ack := viewer.until(isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 0 {
		t.Error("the viewer's update was accepted")
	}
	editor.send(syncpkg.EncodeSyncUpdate(update(t, "k", "editor")))
	if ack := editor.until(isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 1 {
		t.Error("the editor's update was refused")
	}

	if author := <-authors; author != editorID {
		t.Errorf("the update is %s's, want the editor's (%s)", author, editorID)
	}

	// A stateless message is answered to its sender and told to everyone.
	stateless := func(c *hpConn, want string) {
		t.Helper()
		c.until(func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageStateless && string(f.Payload) == want })
	}
	editor.send(syncpkg.EncodeStateless("hello"))
	stateless(editor, "you said hello")
	stateless(editor, "someone said hello")
	stateless(viewer, "someone said hello")

	// Writing is taken from the editor while it is connected, and given
	// back.
	if !srv.SetReadOnly(editorID, true) {
		t.Fatal("the editor's connection was not found")
	}
	editor.send(syncpkg.EncodeSyncUpdate(update(t, "k", "frozen")))
	if ack := editor.until(isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 0 {
		t.Error("a connection made read-only wrote")
	}
	srv.SetReadOnly(editorID, false)
	editor.send(syncpkg.EncodeSyncUpdate(update(t, "k", "thawed")))
	if ack := editor.until(isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 1 {
		t.Error("the editor's update was refused after writing was given back")
	}
	<-authors

	if got := srv.Connections("doc"); len(got) != 2 {
		t.Errorf("connections %v, want two", got)
	}
	// The viewer's link is revoked: its connection ends, the editor's stays.
	if !srv.Disconnect(viewerID, syncpkg.CloseStatusUnauthorized, "revoked") {
		t.Fatal("the viewer's connection was not found")
	}
	for {
		frame, status := viewer.read()
		if frame != nil {
			continue
		}
		if status != syncpkg.CloseStatusUnauthorized {
			t.Errorf("the viewer was closed with %d, want %d", status, syncpkg.CloseStatusUnauthorized)
		}
		break
	}
	editor.send(syncpkg.EncodeStateless("still here"))
	stateless(editor, "you said still here")
	// Gone is gone: neither call finds the viewer any more.
	deadline := time.Now().Add(5 * time.Second)
	for srv.SendStateless(viewerID, "x") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.SendStateless(viewerID, "x") || srv.Disconnect(viewerID, websocket.StatusNormalClosure, "") {
		t.Error("a closed connection is still known")
	}
}

// A client with nothing new answers the server's SyncStep1 with an empty
// update on every connection. That is acknowledged, and is no change: the
// application is not told and nothing is stored.
func TestHocuspocus_AnEmptyUpdateIsNoChange(t *testing.T) {
	changes := make(chan []byte, 4)
	url, _ := startTestServer(t, server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnChange:       func(_ string, u []byte) { changes <- append([]byte(nil), u...) },
	})
	c := hpDial(t, url, "doc")
	c.send(syncpkg.EncodeSyncStep2(ygo.EncodeStateAsUpdate(ygo.NewDoc())))
	if ack := c.until(isType(syncpkg.MessageSyncStatus)); ack.Payload[0] != 1 {
		t.Error("the empty update was refused")
	}
	c.send(syncpkg.EncodeSyncUpdate(update(t, "k", "v")))
	c.until(isType(syncpkg.MessageSyncStatus))
	if got := <-changes; len(got) <= 2 {
		t.Errorf("OnChange got the empty update % x", got)
	}
	select {
	case extra := <-changes:
		t.Errorf("OnChange was called twice, the second time with % x", extra)
	default:
	}
}
