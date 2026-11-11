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
	return &ygoReplica{ygo.NewDocWithOptions(ygo.Options{ClientID: clientID, DisableGC: !gc})}, nil
}

type ygoReplica struct{ doc *ygo.Doc }

// guard reports a panic as an error: one is a finding like any other.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("panic: %v", r)
	}
}

func (r *ygoReplica) Step(step Step) (err error) {
	defer guard(&err)
	return applyStep(r.doc, step)
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
		return ygo.ApplyUpdateV2(r.doc, update)
	}
	return ygo.ApplyUpdate(r.doc, update)
}

func (r *ygoReplica) Dump() (out map[string]any, err error) {
	defer guard(&err)
	return dump(r.doc), nil
}
