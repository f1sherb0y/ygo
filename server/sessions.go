package server

import (
	"github.com/coder/websocket"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// Grant is what OnAuthorize decides for a connection whose token it
// accepts.
type Grant struct {
	// ReadOnly refuses the connection's document updates, as
	// Options.ReadOnly does, and is the scope a Hocuspocus provider is
	// told ("readonly" or "read-write").
	ReadOnly bool
}

// The connection IDs below are the ones OnConnect, OnDisconnect,
// OnAuthorize and OnStatelessFrom are called with. A connection that is
// gone is not an error for any of them: an application learns of a
// disconnect from OnDisconnect, which can come at any time.

// SendStateless sends a Hocuspocus stateless message to one connection, as
// a Hocuspocus server's connection.sendStateless does. It does not wait
// for the write. It reports false when the connection is gone or has not
// opened its session yet (no accepted token).
func (s *Server) SendStateless(connID, payload string) bool {
	c := s.connByID(connID)
	if c == nil || c.waiting.Load() {
		return false
	}
	c.enqueue(syncpkg.EncodeStateless(payload))
	return true
}

// BroadcastStateless sends a Hocuspocus stateless message to every
// connection of a document that has opened its session, as a Hocuspocus
// server's document.broadcastStateless does. A document nobody is
// connected to gets nothing.
func (s *Server) BroadcastStateless(docName, payload string) {
	s.docsMu.Lock()
	state := s.docs[docName]
	s.docsMu.Unlock()
	if state == nil {
		return
	}
	envelope := syncpkg.EncodeStateless(payload)
	for _, c := range state.snapshotConns() {
		c.enqueue(envelope)
	}
}

// Connections returns the IDs of the connections of a document, in no
// order.
func (s *Server) Connections(docName string) []string {
	s.docsMu.Lock()
	state := s.docs[docName]
	s.docsMu.Unlock()
	if state == nil {
		return nil
	}
	conns := state.snapshotConns()
	ids := make([]string, len(conns))
	for i, c := range conns {
		ids[i] = c.idForLog
	}
	return ids
}

// Disconnect closes one connection with the given WebSocket close code
// and reason: what an application does when the access behind a
// connection ends (a revoked link, a session signed out). What is queued
// for the connection is dropped. With code 4401
// (the code a refused token gets) a Hocuspocus provider reports an
// authentication failure and does not reconnect by itself. It reports
// false when the connection is gone already.
func (s *Server) Disconnect(connID string, code websocket.StatusCode, reason string) bool {
	c := s.connByID(connID)
	if c == nil {
		return false
	}
	c.closeFromServer(code, closeReason(reason))
	return true
}

func (s *Server) connByID(id string) *conn {
	if c, ok := s.connsByID.Load(id); ok {
		return c.(*conn)
	}
	return nil
}
