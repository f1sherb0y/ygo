package ygo_test

import (
	"testing"

	"github.com/Deln0r/ygo"
)

// Undo puts a subdocument back where a write replaced it, as a reference
// with the same GUID, as yjs does.
func TestUndo_RestoresASubdocument(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	um := ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1}, m)

	txn := d.WriteTxn()
	guid := m.SetDoc(txn, "k").GUID()
	txn.Commit()
	txn = d.WriteTxn()
	m.Set(txn, "k", "overwritten")
	txn.Commit()
	if _, ok := m.GetDoc(d, "k"); ok {
		t.Fatal("the key still holds the subdocument after it was overwritten")
	}

	if !um.Undo() {
		t.Fatal("nothing to undo")
	}
	sub, ok := m.GetDoc(d, "k")
	if !ok || sub.GUID() != guid {
		t.Fatalf("after undo the key holds %v, want the subdocument %s", m.Get("k"), guid)
	}

	// And another replica that gets all of it sees the same reference.
	peer := ygo.NewDoc()
	if err := ygo.ApplyUpdate(peer, ygo.EncodeStateAsUpdate(d)); err != nil {
		t.Fatal(err)
	}
	if sub, ok := ygo.NewMap(peer, "m").GetDoc(peer, "k"); !ok || sub.GUID() != guid {
		t.Error("the peer does not see the restored subdocument")
	}
}
