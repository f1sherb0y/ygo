package conformance

import (
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/Deln0r/ygo"
)

// Presence is one awareness instance of one implementation: the state of
// its own client and what it has heard of the others.
type Presence interface {
	// Set sets the local state; nil removes it.
	Set(state any) error
	// Encode encodes every client the instance knows, the removed ones
	// included.
	Encode() ([]byte, error)
	Apply(update []byte) error
	// States returns the state of every client that has one, by client ID.
	States() (map[string]any, error)
}

// PresenceImplementation makes awareness instances.
type PresenceImplementation interface {
	Name() string
	NewPresence(clientID uint64) (Presence, error)
}

type ygoPresence struct{ a *ygo.Awareness }

func (ygoImplementation) NewPresence(clientID uint64) (Presence, error) {
	return ygoPresence{ygo.NewAwareness(clientID)}, nil
}

func (p ygoPresence) Set(state any) (err error) {
	defer guard(&err)
	if state == nil {
		p.a.RemoveLocalState()
		return nil
	}
	return p.a.SetLocalStateJSON(state)
}

func (p ygoPresence) Encode() (out []byte, err error) {
	defer guard(&err)
	return p.a.Encode(nil), nil
}

func (p ygoPresence) Apply(update []byte) (err error) {
	defer guard(&err)
	_, err = p.a.Apply(update, "remote")
	return err
}

func (p ygoPresence) States() (out map[string]any, err error) {
	defer guard(&err)
	out = map[string]any{}
	for client, raw := range p.a.States() {
		var state any
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		out[strconv.FormatUint(client, 10)] = state
	}
	return out, nil
}

type driverPresence struct {
	d  *Driver
	id int
}

func (d *Driver) NewPresence(clientID uint64) (Presence, error) {
	d.next++
	_, err := d.call(map[string]any{"cmd": "anew", "id": d.next, "client": clientID})
	return driverPresence{d, d.next}, err
}

func (p driverPresence) Set(state any) error {
	_, err := p.d.call(map[string]any{"cmd": "aset", "id": p.id, "state": state})
	return err
}

func (p driverPresence) Encode() ([]byte, error) {
	a, err := p.d.call(map[string]any{"cmd": "aenc", "id": p.id})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(a.Hex)
}

func (p driverPresence) Apply(update []byte) error {
	_, err := p.d.call(map[string]any{"cmd": "aapply", "id": p.id, "update": hex.EncodeToString(update)})
	return err
}

func (p driverPresence) States() (map[string]any, error) {
	a, err := p.d.call(map[string]any{"cmd": "astates", "id": p.id})
	return a.Dump, err
}
