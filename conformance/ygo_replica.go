package conformance

import (
	"fmt"

	"github.com/Deln0r/ygo"
)

// Ygo is ygo as an implementation, in this process.
var Ygo Implementation = ygoImplementation{}

type ygoImplementation struct{}

func (ygoImplementation) Name() string { return "ygo" }

func (ygoImplementation) New(clientID uint64, gc bool) (Replica, error) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID, DisableGC: !gc})
	// The roots get their types now, as the drivers of yjs and yrs give
	// them: yjs cleans up formatting after remote changes only in a text
	// it knows to be one, and ygo does the same.
	text, arr, m, xml := ygo.NewText(d, "text"), ygo.NewArray(d, "arr"), ygo.NewMap(d, "map"), ygo.NewXmlFragment(d, "xml")
	// Every transaction of the replica's own is one step to undo; what
	// arrives from others (origin remoteOrigin) is none.
	um := ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1}, text, arr, m, xml)
	return &ygoReplica{doc: d, undo: um}, nil
}

// remoteOrigin is the origin of the updates a replica is given.
const remoteOrigin = "remote"

func (ygoImplementation) Merge(updates [][]byte, version int) (merged []byte, err error) {
	defer guard(&err)
	if version == 2 {
		return ygo.MergeUpdatesV2(updates)
	}
	return ygo.MergeUpdates(updates)
}

type ygoReplica struct {
	doc       *ygo.Doc
	undo      *ygo.UndoManager
	positions []ygo.RelativePosition
	snapshots []ygo.Snapshot
}

// guard reports a panic as an error: one is a finding like any other.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("panic: %v", r)
	}
}

func (r *ygoReplica) Step(step Step) (err error) {
	defer guard(&err)
	switch step.Op {
	case "undo":
		r.undo.Undo()
	case "redo":
		r.undo.Redo()
	case "rpos":
		text := ygo.NewText(r.doc, "text")
		rtxn := r.doc.ReadTxn()
		length := text.Length()
		rtxn.Close()
		pos, err := ygo.CreateRelativePositionFromTypeIndex(text, at(step.N, length), int64(-step.K))
		if err != nil {
			return err
		}
		r.positions = append(r.positions, pos)
	case "snap":
		if !r.doc.GC() {
			r.snapshots = append(r.snapshots, ygo.CreateSnapshot(r.doc))
		}
	default:
		return applyStep(r.doc, step)
	}
	return nil
}

// local is what the replica holds for itself: each position as it is
// encoded and where it is now, each snapshot as the text and the array
// it restores.
func (r *ygoReplica) local() (map[string]any, error) {
	positions := []any{}
	for _, pos := range r.positions {
		raw, err := ygo.EncodeRelativePosition(pos)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{"bytes": fmt.Sprintf("%x", raw), "index": nil}
		if abs, ok := ygo.CreateAbsolutePositionFromRelativePosition(r.doc, pos); ok {
			entry["index"], entry["assoc"] = abs.Index, abs.Assoc
		}
		positions = append(positions, entry)
	}
	snapshots := []any{}
	for _, snap := range r.snapshots {
		// Through its bytes, as a snapshot kept somewhere would be.
		decoded, err := ygo.DecodeSnapshot(ygo.EncodeSnapshot(snap))
		if err != nil {
			return nil, err
		}
		then, err := ygo.RestoreSnapshot(r.doc, decoded)
		if err != nil {
			return nil, err
		}
		restored := dump(then)
		snapshots = append(snapshots, map[string]any{"text": restored["text"], "arr": restored["arr"], "map": restored["map"]})
	}
	return map[string]any{"positions": positions, "snapshots": snapshots}, nil
}

func (r *ygoReplica) StateVector() ([]byte, error) { return ygo.EncodeStateVector(r.doc), nil }

func (r *ygoReplica) Diff(sv []byte, version int) (diff []byte, err error) {
	defer guard(&err)
	if version == 2 {
		return ygo.EncodeDiffV2(r.doc, sv)
	}
	return ygo.EncodeDiff(r.doc, sv)
}

func (r *ygoReplica) Apply(update []byte, version int) (err error) {
	defer guard(&err)
	if version == 2 {
		return ygo.ApplyUpdateV2WithOrigin(r.doc, update, remoteOrigin)
	}
	return ygo.ApplyUpdateWithOrigin(r.doc, update, remoteOrigin)
}

func (r *ygoReplica) Dump() (out map[string]any, err error) {
	defer guard(&err)
	out = dump(r.doc)
	if out[localKey], err = r.local(); err != nil {
		return nil, err
	}
	return out, nil
}
