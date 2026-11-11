package server_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/server"
)

// stressScale multiplies the size of the stress tests: YGO_STRESS=10 for a
// soak, 1 (the default) for the ordinary test run.
func stressScale() int {
	if n, err := strconv.Atoi(os.Getenv("YGO_STRESS")); err == nil && n > 0 {
		return n
	}
	return 1
}

// editor is one client of the stress test: a document, its text and the
// connection that syncs it.
type editor struct {
	doc  *ygo.Doc
	text *ygo.Text
	conn *client.Client
}

func (e *editor) connect(t *testing.T, url, name string, hocuspocus bool) {
	t.Helper()
	conn, err := client.New(client.Options{URL: url, DocName: name, Doc: e.doc, Hocuspocus: hocuspocus})
	if err != nil {
		t.Error(err)
		return
	}
	if err := conn.Connect(context.Background()); err != nil {
		t.Error(err)
	}
	e.conn = conn
}

// content returns the text and the state vector. The two are read one after
// the other, not under one read transaction: EncodeStateVector opens its
// own, and a second read lock behind a waiting writer would never be
// granted.
func (e *editor) content() (string, string) {
	txn := e.doc.ReadTxn()
	text := e.text.String()
	txn.Close()
	return text, fmt.Sprintf("%x", ygo.EncodeStateVector(e.doc))
}

// TestStress_ConcurrentEditorsConverge has several clients per document type
// into and delete from the same text at once, some of them dropping their
// connection in the middle, over y-websocket and over Hocuspocus frames.
// Every client of a document, and one that only joins afterwards, must end
// with the same text.
func TestStress_ConcurrentEditorsConverge(t *testing.T) {
	scale := stressScale()
	const documents, perDocument = 4, 6
	edits := 120 * scale
	for _, hocuspocus := range []bool{false, true} {
		name := "y-websocket"
		if hocuspocus {
			name = "hocuspocus"
		}
		t.Run(name, func(t *testing.T) {
			url, _ := startTestServer(t, server.Options{Hocuspocus: hocuspocus, OriginPatterns: []string{"*"}})
			rooms := make([][]*editor, documents)
			var wg sync.WaitGroup
			for d := range rooms {
				room := fmt.Sprintf("stress-%d", d)
				for c := range perDocument {
					e := &editor{doc: ygo.NewDoc()}
					e.text = ygo.NewText(e.doc, "t")
					e.connect(t, url, room, hocuspocus)
					rooms[d] = append(rooms[d], e)
					wg.Add(1)
					go func() {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(uint64(d), uint64(c)))
						for i := range edits {
							txn := e.doc.WriteTxn()
							length := e.text.Length()
							var err error
							if length > 20 && rng.IntN(3) == 0 {
								at := rng.Uint64N(length)
								err = e.text.Delete(txn, at, min(1+rng.Uint64N(3), length-at))
							} else {
								err = e.text.Insert(txn, rng.Uint64N(length+1), fmt.Sprintf("%c%d ", 'a'+c, i%10))
							}
							txn.Commit()
							if err != nil {
								t.Errorf("edit: %v", err)
								return
							}
							// A third of the clients lose their connection
							// halfway and come back with what they typed
							// meanwhile.
							if i == edits/2 && c%3 == 0 {
								_ = e.conn.Close()
								e.connect(t, url, room, hocuspocus)
							}
							if i%16 == 0 {
								time.Sleep(time.Millisecond)
							}
						}
					}()
				}
			}
			wg.Wait()
			if t.Failed() {
				return
			}

			for d, room := range rooms {
				// A client that was not there for any of it.
				late := &editor{doc: ygo.NewDoc()}
				late.text = ygo.NewText(late.doc, "t")
				late.connect(t, url, fmt.Sprintf("stress-%d", d), hocuspocus)
				room = append(room, late)

				deadline := time.Now().Add(30 * time.Second)
				for {
					text, clocks := room[0].content()
					same := text != ""
					for _, e := range room[1:] {
						if other, otherClocks := e.content(); other != text || otherClocks != clocks {
							same = false
						}
					}
					if same {
						break
					}
					if time.Now().After(deadline) {
						for i, e := range room {
							text, clocks := e.content()
							t.Logf("document %d, client %d: %d bytes, clocks %s", d, i, len(text), clocks)
						}
						t.Fatalf("document %d did not converge", d)
					}
					time.Sleep(20 * time.Millisecond)
				}
				for _, e := range room {
					_ = e.conn.Close()
				}
			}
		})
	}
}
