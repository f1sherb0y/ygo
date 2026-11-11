// backend is an application server of the kind the ygo server is meant to
// carry, small enough to read in one sitting and driven from outside by
// ../backend.mjs with stock Hocuspocus providers. It keeps the contract of
// an existing Hocuspocus-compatible collaboration backend with accounts,
// share links and session replay, message for message:
//
//   - a token (a JWT; the signature is not checked here) says who a
//     connection is, until when, and whether it may write. A new token on
//     an open connection renews it for the same user and is refused for
//     another (OnAuthorize);
//   - when a session opens the client is sent its colour, the state of
//     the store and the language of the room, and the room is told who is
//     in it (OnSessionOpen, SendStateless, BroadcastStateless);
//   - every accepted update is queued for a writer that stores it as a
//     row with a sequence number and its author (OnChangeFrom); a
//     document is loaded from a snapshot and the rows after it
//     (OnLoadDocument);
//   - "durability-barrier" is answered with "durability-ack" once
//     everything queued before it is stored, and "persistence-status"
//     tells the room how much is waiting;
//   - "presence" registers the client's ID, and "presence-state" lists
//     the members; a "heartbeat" goes out on a timer;
//   - ending a room tells it "room-status" and takes writing away from
//     everyone in it (SetReadOnly); revoking a user closes their
//     connections with 4403 "Access revoked" (Disconnect).
//
// The store is a directory of files, so that the test can stop the process
// and start it again on what it wrote.
//
// HTTP, for the test only: GET /rows?doc=, POST /checkpoint?doc=,
// POST /revoke?user=, POST /end?doc=, POST /language?doc=&value=,
// POST /stall?on=1 (the writer waits).
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/server"
)

// store keeps a document as a directory: one file per update row, named
// by its sequence number and its author, and a snapshot with the sequence
// number it covers.
type store struct {
	mu   sync.Mutex
	dir  string
	seqs map[string]int // last sequence number per document
}

type row struct {
	Seq    int    `json:"seq"`
	User   string `json:"userId"`
	Update string `json:"update"`
	raw    []byte
}

func (s *store) path(doc string) string { return filepath.Join(s.dir, url.PathEscape(doc)) }

// rows returns the stored rows of doc after sequence number after, in
// order. The caller holds mu.
func (s *store) rows(doc string, after int) []row {
	entries, _ := os.ReadDir(s.path(doc))
	var out []row
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".row")
		if !ok {
			continue
		}
		number, user, _ := strings.Cut(name, "-")
		seq, err := strconv.Atoi(number)
		if err != nil || seq <= after {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.path(doc), e.Name()))
		if err != nil {
			log.Fatal(err)
		}
		user, _ = url.PathUnescape(user)
		out = append(out, row{Seq: seq, User: user, Update: base64.StdEncoding.EncodeToString(raw), raw: raw})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s *store) snapshot(doc string) ([]byte, int) {
	raw, err := os.ReadFile(filepath.Join(s.path(doc), "snapshot"))
	if err != nil {
		return nil, 0
	}
	mark, _ := os.ReadFile(filepath.Join(s.path(doc), "checkpoint"))
	seq, _ := strconv.Atoi(string(mark))
	return raw, seq
}

// merged is the snapshot and every row after it as one update, with the
// last sequence number. The caller holds mu.
func (s *store) merged(doc string) ([]byte, int, error) {
	snapshot, last := s.snapshot(doc)
	var updates [][]byte
	if snapshot != nil {
		updates = append(updates, snapshot)
	}
	for _, r := range s.rows(doc, last) {
		updates = append(updates, r.raw)
		last = r.Seq
	}
	update, err := ygo.MergeUpdates(updates)
	return update, last, err
}

func (s *store) load(doc string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update, last, err := s.merged(doc)
	s.seqs[doc] = last
	return update, err
}

func (s *store) append(doc, user string, update []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.path(doc), 0o755); err != nil {
		log.Fatal(err)
	}
	s.seqs[doc]++
	name := fmt.Sprintf("%08d-%s.row", s.seqs[doc], url.PathEscape(user))
	if err := os.WriteFile(filepath.Join(s.path(doc), name), update, 0o644); err != nil {
		log.Fatal(err)
	}
	return s.seqs[doc]
}

func (s *store) checkpoint(doc string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	update, last, err := s.merged(doc)
	if err != nil || last == 0 {
		return last, err
	}
	if err := os.WriteFile(filepath.Join(s.path(doc), "snapshot"), update, 0o644); err != nil {
		return 0, err
	}
	return last, os.WriteFile(filepath.Join(s.path(doc), "checkpoint"), []byte(strconv.Itoa(last)), 0o644)
}

// claims is what a token says.
type claims struct {
	Type     string `json:"type"`
	UserID   string `json:"userId"`
	Username string `json:"username"`
	ReadOnly bool   `json:"ro"`
	Exp      int64  `json:"exp"`
}

func parseToken(token string) (claims, error) {
	var c claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, errors.New("Invalid token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil || c.UserID == "" {
		return c, errors.New("Invalid token")
	}
	if c.Exp <= time.Now().Unix() {
		return c, errors.New("Token expired")
	}
	return c, nil
}

// member is one open connection.
type member struct {
	doc      string
	user     claims
	slot     int
	clientID uint64
	open     bool
}

// work is one thing the writer does in order: store an update, or answer
// a barrier.
type work struct {
	doc, conn, user string
	update          []byte
	barrier         string
}

type backend struct {
	srv   *server.Server
	store *store

	mu       sync.Mutex
	members  map[string]*member // by connection ID
	revoked  map[string]bool
	ended    map[string]bool
	language map[string]string
	pending  map[string]int // queued work per document

	queue chan work
	stall sync.RWMutex // held for writing while the test stalls the writer
}

var colours = []string{"#e11d48", "#2563eb", "#16a34a", "#d97706", "#7c3aed", "#0891b2"}

func (b *backend) authorize(connID, doc, token string) (server.Grant, error) {
	c, err := parseToken(token)
	if err != nil {
		return server.Grant{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.revoked[c.UserID] {
		return server.Grant{}, errors.New("Access revoked")
	}
	if m := b.members[connID]; m != nil {
		// A renewal: the same user goes on, another needs a new socket.
		if m.user.UserID != c.UserID {
			return server.Grant{}, errors.New("Access revoked")
		}
		m.user = c
		return server.Grant{ReadOnly: c.ReadOnly || b.ended[doc]}, nil
	}
	used := map[int]bool{}
	for _, m := range b.members {
		if m.doc == doc {
			used[m.slot] = true
		}
	}
	slot := 0
	for used[slot] {
		slot++
	}
	b.members[connID] = &member{doc: doc, user: c, slot: slot}
	return server.Grant{ReadOnly: c.ReadOnly || b.ended[doc]}, nil
}

func (b *backend) say(connID string, message map[string]any) {
	raw, _ := json.Marshal(message)
	b.srv.SendStateless(connID, string(raw))
}

func (b *backend) tell(doc string, message map[string]any) {
	raw, _ := json.Marshal(message)
	b.srv.BroadcastStateless(doc, string(raw))
}

func (b *backend) status(doc string) map[string]any {
	b.mu.Lock()
	pending := b.pending[doc]
	b.mu.Unlock()
	b.store.mu.Lock()
	seq := b.store.seqs[doc]
	b.store.mu.Unlock()
	return map[string]any{"type": "persistence-status", "pending": pending, "failed": false, "durableSeq": seq, "protocol": 1}
}

func (b *backend) presence(doc string) map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	members := []map[string]any{}
	ids := b.srv.Connections(doc)
	sort.Strings(ids)
	for _, id := range ids {
		m := b.members[id]
		if m == nil || !m.open {
			continue
		}
		members = append(members, map[string]any{
			"connectionId": id, "clientId": m.clientID, "id": m.user.UserID, "username": m.user.Username,
			"colorSlot": m.slot, "color": colours[m.slot%len(colours)], "colorLight": colours[m.slot%len(colours)] + "33",
		})
	}
	return map[string]any{"type": "presence-state", "members": members}
}

func (b *backend) sessionOpen(connID, doc string) {
	b.mu.Lock()
	m := b.members[connID]
	if m == nil {
		b.mu.Unlock()
		return
	}
	m.open = true
	slot, language := m.slot, b.language[doc]
	b.mu.Unlock()
	b.say(connID, map[string]any{"type": "session-color", "slot": slot, "color": colours[slot%len(colours)], "colorLight": colours[slot%len(colours)] + "33"})
	b.say(connID, b.status(doc))
	b.tell(doc, b.presence(doc))
	if language != "" {
		b.say(connID, map[string]any{"type": "room-language", "language": language})
	}
}

func (b *backend) enqueue(w work) {
	b.mu.Lock()
	b.pending[w.doc]++
	b.mu.Unlock()
	b.queue <- w
}

// write is the one goroutine that stores: rows in the order they were
// accepted, and a barrier's answer once everything before it is stored.
func (b *backend) write() {
	for w := range b.queue {
		b.stall.RLock()
		if w.barrier != "" {
			b.store.mu.Lock()
			seq := b.store.seqs[w.doc]
			b.store.mu.Unlock()
			b.say(w.conn, map[string]any{"type": "durability-ack", "id": w.barrier, "durableSeq": seq})
		} else {
			b.store.append(w.doc, w.user, w.update)
		}
		b.stall.RUnlock()
		b.mu.Lock()
		b.pending[w.doc]--
		b.mu.Unlock()
		b.tell(w.doc, b.status(w.doc))
	}
}

func (b *backend) stateless(connID, doc, payload string) {
	var msg struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		ClientID uint64 `json:"clientId"`
	}
	if json.Unmarshal([]byte(payload), &msg) != nil {
		return
	}
	switch msg.Type {
	case "presence":
		b.mu.Lock()
		if m := b.members[connID]; m != nil {
			m.clientID = msg.ClientID
		}
		b.mu.Unlock()
		b.tell(doc, b.presence(doc))
	case "durability-barrier":
		if msg.ID != "" && len(msg.ID) <= 100 {
			b.enqueue(work{doc: doc, conn: connID, barrier: msg.ID})
		}
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	data := flag.String("data", "", "directory for the stored documents")
	heartbeat := flag.Duration("heartbeat", 10*time.Second, "interval of the heartbeat message")
	flag.Parse()
	if *data == "" {
		log.Fatal("-data is required")
	}
	b := &backend{
		store:   &store{dir: *data, seqs: map[string]int{}},
		members: map[string]*member{}, revoked: map[string]bool{}, ended: map[string]bool{},
		language: map[string]string{}, pending: map[string]int{},
		queue: make(chan work, 4096),
	}
	b.srv = server.New(server.Options{
		Hocuspocus:      true,
		OriginPatterns:  []string{"*"},
		OnLoadDocument:  b.store.load,
		OnAuthorize:     b.authorize,
		OnSessionOpen:   b.sessionOpen,
		OnStatelessFrom: b.stateless,
		OnChangeFrom: func(connID, doc string, update []byte) {
			b.mu.Lock()
			user := ""
			if m := b.members[connID]; m != nil {
				user = m.user.UserID
			}
			b.mu.Unlock()
			b.enqueue(work{doc: doc, conn: connID, user: user, update: append([]byte(nil), update...)})
			b.tell(doc, b.status(doc))
		},
		OnDisconnect: func(connID, doc string) {
			b.mu.Lock()
			delete(b.members, connID)
			b.mu.Unlock()
			b.tell(doc, b.presence(doc))
		},
	})
	go b.write()
	go func() {
		for range time.Tick(*heartbeat) {
			b.mu.Lock()
			docs := map[string]bool{}
			var expired []string
			for id, m := range b.members {
				docs[m.doc] = true
				if m.user.Exp <= time.Now().Unix() {
					expired = append(expired, id)
				}
			}
			b.mu.Unlock()
			for _, id := range expired {
				b.srv.Disconnect(id, 1000, "Session expired")
			}
			for doc := range docs {
				b.tell(doc, b.presence(doc))
				b.tell(doc, map[string]any{"type": "heartbeat"})
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/api/ws", b.srv.Handler())
	query := func(r *http.Request, key string) string { return r.URL.Query().Get(key) }
	mux.HandleFunc("/rows", func(w http.ResponseWriter, r *http.Request) {
		b.store.mu.Lock()
		rows := b.store.rows(query(r, "doc"), 0)
		b.store.mu.Unlock()
		if rows == nil {
			rows = []row{}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		seq, err := b.store.checkpoint(query(r, "doc"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, seq)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		user := query(r, "user")
		b.mu.Lock()
		b.revoked[user] = true
		var ids []string
		for id, m := range b.members {
			if m.user.UserID == user {
				ids = append(ids, id)
			}
		}
		b.mu.Unlock()
		for _, id := range ids {
			b.srv.Disconnect(id, 4403, "Access revoked")
		}
		fmt.Fprint(w, len(ids))
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		doc := query(r, "doc")
		b.mu.Lock()
		b.ended[doc] = true
		b.mu.Unlock()
		for _, id := range b.srv.Connections(doc) {
			b.srv.SetReadOnly(id, true)
		}
		b.tell(doc, map[string]any{"type": "room-status", "status": "ended", "endedAt": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("/language", func(w http.ResponseWriter, r *http.Request) {
		doc, value := query(r, "doc"), query(r, "value")
		b.mu.Lock()
		b.language[doc] = value
		b.mu.Unlock()
		b.tell(doc, map[string]any{"type": "room-language", "language": value})
	})
	mux.HandleFunc("/stall", func(w http.ResponseWriter, r *http.Request) {
		if query(r, "on") == "1" {
			b.stall.Lock()
		} else {
			b.stall.Unlock()
		}
	})
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", listener.Addr())
	log.Fatal(http.Serve(listener, mux))
}
