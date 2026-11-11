// backend is an application server of the kind the ygo server is meant to
// carry, small enough to read in one sitting and driven from outside by
// ../backend.mjs with stock Hocuspocus providers. It does what a
// collaboration backend with accounts, share links and session replay does
// around the CRDT:
//
//   - a token decides who a connection is and whether it may write: the
//     same document has editors and viewers (OnAuthorize);
//   - every accepted update is stored as a row of its own with a sequence
//     number, for replay (OnChange), and a document is loaded from a
//     snapshot plus the rows after it (OnLoadDocument);
//   - a client asks over a stateless message whether what it sent is
//     stored and is answered with the sequence number (OnStatelessFrom,
//     SendStateless);
//   - the room is told things over stateless messages (BroadcastStateless);
//   - access that ends closes the connections that had it (Disconnect).
//
// The store is a directory of files, so that the test can stop the process
// and start it again on what it wrote.
//
// Tokens: "edit:<user>" and "view:<user>". HTTP, for the test only:
// GET /rows?doc=, POST /checkpoint?doc=, POST /revoke?user=,
// POST /announce?doc=&text=.
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

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/server"
)

// store keeps a document as a directory: one file per update row, named
// by its sequence number, and a snapshot with the sequence number it
// covers.
type store struct {
	mu   sync.Mutex
	dir  string
	seqs map[string]int // last sequence number per document
}

func (s *store) path(doc string) string {
	return filepath.Join(s.dir, url.PathEscape(doc))
}

// rows returns the stored updates of doc after sequence number after, in
// order, and the last sequence number. The caller holds mu.
func (s *store) rows(doc string, after int) (updates [][]byte, seqs []int) {
	entries, _ := os.ReadDir(s.path(doc))
	for _, e := range entries {
		seq, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".row"))
		if err != nil || !strings.HasSuffix(e.Name(), ".row") || seq <= after {
			continue
		}
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	for _, seq := range seqs {
		raw, err := os.ReadFile(filepath.Join(s.path(doc), fmt.Sprintf("%08d.row", seq)))
		if err != nil {
			log.Fatal(err)
		}
		updates = append(updates, raw)
	}
	return updates, seqs
}

// snapshot returns the snapshot of doc and the sequence number it covers.
func (s *store) snapshot(doc string) ([]byte, int) {
	raw, err := os.ReadFile(filepath.Join(s.path(doc), "snapshot"))
	if err != nil {
		return nil, 0
	}
	mark, _ := os.ReadFile(filepath.Join(s.path(doc), "checkpoint"))
	seq, _ := strconv.Atoi(string(mark))
	return raw, seq
}

// load is the document as stored: the snapshot and every row after it.
func (s *store) load(doc string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, checkpoint := s.snapshot(doc)
	updates, seqs := s.rows(doc, checkpoint)
	last := checkpoint
	if len(seqs) > 0 {
		last = seqs[len(seqs)-1]
	}
	s.seqs[doc] = last
	if snapshot != nil {
		updates = append([][]byte{snapshot}, updates...)
	}
	return ygo.MergeUpdates(updates)
}

// append stores one update as the next row and returns its number.
func (s *store) append(doc string, update []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.path(doc), 0o755); err != nil {
		log.Fatal(err)
	}
	s.seqs[doc]++
	seq := s.seqs[doc]
	if err := os.WriteFile(filepath.Join(s.path(doc), fmt.Sprintf("%08d.row", seq)), update, 0o644); err != nil {
		log.Fatal(err)
	}
	return seq
}

// checkpoint writes a snapshot of everything stored so far.
func (s *store) checkpoint(doc string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot, checkpoint := s.snapshot(doc)
	updates, seqs := s.rows(doc, checkpoint)
	if len(seqs) == 0 {
		return checkpoint, nil
	}
	if snapshot != nil {
		updates = append([][]byte{snapshot}, updates...)
	}
	merged, err := ygo.MergeUpdates(updates)
	if err != nil {
		return 0, err
	}
	last := seqs[len(seqs)-1]
	if err := os.WriteFile(filepath.Join(s.path(doc), "snapshot"), merged, 0o644); err != nil {
		return 0, err
	}
	return last, os.WriteFile(filepath.Join(s.path(doc), "checkpoint"), []byte(strconv.Itoa(last)), 0o644)
}

// sessions is who is behind each connection.
type sessions struct {
	mu      sync.Mutex
	users   map[string]string // connection ID to user
	revoked map[string]bool
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	data := flag.String("data", "", "directory for the stored documents")
	flag.Parse()
	if *data == "" {
		log.Fatal("-data is required")
	}
	st := &store{dir: *data, seqs: map[string]int{}}
	who := &sessions{users: map[string]string{}, revoked: map[string]bool{}}

	var srv *server.Server
	srv = server.New(server.Options{
		Hocuspocus:     true,
		OriginPatterns: []string{"*"},
		OnLoadDocument: st.load,
		OnChange: func(doc string, update []byte) {
			st.append(doc, update)
		},
		OnAuthorize: func(connID, doc, token string) (server.Grant, error) {
			kind, user, ok := strings.Cut(token, ":")
			if !ok || user == "" || (kind != "edit" && kind != "view") {
				return server.Grant{}, errors.New("Invalid token")
			}
			who.mu.Lock()
			defer who.mu.Unlock()
			if who.revoked[user] {
				return server.Grant{}, errors.New("Access revoked")
			}
			who.users[connID] = user
			return server.Grant{ReadOnly: kind == "view"}, nil
		},
		OnDisconnect: func(connID, _ string) {
			who.mu.Lock()
			delete(who.users, connID)
			who.mu.Unlock()
		},
		OnStatelessFrom: func(connID, doc, payload string) {
			var msg struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if json.Unmarshal([]byte(payload), &msg) != nil {
				return
			}
			switch msg.Type {
			case "durability-barrier":
				// Every update this connection sent before the barrier
				// went through OnChange on this goroutine: it is stored.
				st.mu.Lock()
				seq := st.seqs[doc]
				st.mu.Unlock()
				reply, _ := json.Marshal(map[string]any{"type": "durability-barrier", "id": msg.ID, "seq": seq})
				srv.SendStateless(connID, string(reply))
			case "presence":
				who.mu.Lock()
				var users []string
				for _, id := range srv.Connections(doc) {
					if user, ok := who.users[id]; ok {
						users = append(users, user)
					}
				}
				who.mu.Unlock()
				sort.Strings(users)
				note, _ := json.Marshal(map[string]any{"type": "presence", "users": users})
				srv.BroadcastStateless(doc, string(note))
			}
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/api/ws", srv.Handler())
	mux.HandleFunc("/rows", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		updates, seqs := st.rows(r.URL.Query().Get("doc"), 0)
		st.mu.Unlock()
		type row struct {
			Seq    int    `json:"seq"`
			Update string `json:"update"`
		}
		out := make([]row, len(updates))
		for i, u := range updates {
			out[i] = row{seqs[i], base64.StdEncoding.EncodeToString(u)}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		seq, err := st.checkpoint(r.URL.Query().Get("doc"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, seq)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		user := r.URL.Query().Get("user")
		who.mu.Lock()
		who.revoked[user] = true
		var ids []string
		for id, u := range who.users {
			if u == user {
				ids = append(ids, id)
			}
		}
		who.mu.Unlock()
		for _, id := range ids {
			srv.Disconnect(id, 4401, "Access revoked")
		}
		fmt.Fprint(w, len(ids))
	})
	mux.HandleFunc("/announce", func(w http.ResponseWriter, r *http.Request) {
		note, _ := json.Marshal(map[string]any{"type": "announce", "text": r.URL.Query().Get("text")})
		srv.BroadcastStateless(r.URL.Query().Get("doc"), string(note))
	})

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s", listener.Addr())
	log.Fatal(http.Serve(listener, mux))
}
