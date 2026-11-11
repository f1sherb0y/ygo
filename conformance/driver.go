package conformance

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Implementation makes replicas: ygo itself, or a reference implementation
// in a child process.
type Implementation interface {
	Name() string
	New(clientID uint64, gc bool) (Replica, error)
}

// Replica is one document of one implementation.
type Replica interface {
	// Step runs one editing step as a transaction.
	Step(Step) error
	StateVector() ([]byte, error)
	// Diff encodes what a peer with the given state vector lacks, as a
	// version 1 or 2 update.
	Diff(sv []byte, version int) ([]byte, error)
	Apply(update []byte, version int) error
	// Dump returns the content of the four root types.
	Dump() (map[string]any, error)
}

// Driver is a reference implementation in a child process that reads one
// JSON command per line and answers with one JSON line (yjs/driver.mjs,
// yrs/src/main.rs).
type Driver struct {
	name string
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

// StartDriver starts a driver process in dir.
func StartDriver(name, dir string, command ...string) (*Driver, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Driver{name: name, cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}, nil
}

// Close ends the process.
func (d *Driver) Close() {
	d.in.Close()
	_ = d.cmd.Wait()
}

func (d *Driver) Name() string { return d.name }

type answer struct {
	Hex   string         `json:"hex"`
	Dump  map[string]any `json:"dump"`
	Error string         `json:"error"`
}

func (d *Driver) call(request map[string]any) (answer, error) {
	var a answer
	line, err := json.Marshal(request)
	if err != nil {
		return a, err
	}
	if _, err := d.in.Write(append(line, '\n')); err != nil {
		return a, fmt.Errorf("%s: %w", d.name, err)
	}
	reply, err := d.out.ReadBytes('\n')
	if err != nil {
		return a, fmt.Errorf("%s: %w", d.name, err)
	}
	if err := json.Unmarshal(reply, &a); err != nil {
		return a, err
	}
	if a.Error != "" {
		return a, errors.New(a.Error)
	}
	return a, nil
}

// Reset drops every document the driver holds.
func (d *Driver) Reset() {
	_, _ = d.call(map[string]any{"cmd": "reset"})
}

func (d *Driver) New(clientID uint64, gc bool) (Replica, error) {
	d.next++
	_, err := d.call(map[string]any{"cmd": "new", "id": d.next, "client": clientID, "gc": gc})
	return &driverReplica{d, d.next}, err
}

type driverReplica struct {
	d  *Driver
	id int
}

func (r *driverReplica) Step(step Step) error {
	_, err := r.d.call(map[string]any{"cmd": "step", "id": r.id, "step": step})
	return err
}

func (r *driverReplica) StateVector() ([]byte, error) {
	a, err := r.d.call(map[string]any{"cmd": "sv", "id": r.id})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(a.Hex)
}

func (r *driverReplica) Diff(sv []byte, version int) ([]byte, error) {
	a, err := r.d.call(map[string]any{"cmd": "diff", "id": r.id, "sv": hex.EncodeToString(sv), "v": version})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(a.Hex)
}

func (r *driverReplica) Apply(update []byte, version int) error {
	_, err := r.d.call(map[string]any{"cmd": "apply", "id": r.id, "update": hex.EncodeToString(update), "v": version})
	return err
}

func (r *driverReplica) Dump() (map[string]any, error) {
	a, err := r.d.call(map[string]any{"cmd": "dump", "id": r.id})
	return a.Dump, err
}
