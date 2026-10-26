package cachebrokerlab

import (
	"errors"
	"os/exec"
	"testing"
)

// Native suites supply real kernel containment. This boundary double injects
// an active orphan or a failed kernel query, independently of the leader's exit.
type proofTree struct {
	active uint32
	err    error
	starts int
}

func (p *proofTree) Start(*exec.Cmd) error   { p.starts++; return nil }
func (p *proofTree) Active() (uint32, error) { return p.active, p.err }

func TestSupervisorRetainsActiveDescendantsAndSealsLaunches(t *testing.T) {
	s, err := openSupervisor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	binding := RunBinding{RunID: "task-run", LeaseID: "lease", Generation: 1, Identity: "uid-501"}
	tree := &proofTree{active: 1}
	if err := s.Launch(binding, &exec.Cmd{}, tree); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Quiescent(binding); err != nil || done {
		t.Fatalf("active orphan authorized terminal: %v %v", done, err)
	}
	if err := s.Launch(binding, &exec.Cmd{}, tree); err == nil || tree.starts != 1 {
		t.Fatal("sealed launch was replayed")
	}
	tree.err = errors.New("kernel census unavailable")
	if done, err := s.Quiescent(binding); err == nil || done {
		t.Fatal("query failure authorized terminal")
	}
	tree.err = nil
	tree.active = 0
	if done, err := s.Quiescent(binding); err != nil || !done {
		t.Fatalf("quiescent sealed domain rejected: %v %v", done, err)
	}
	wrong := binding
	wrong.LeaseID = "other"
	if done, err := s.Quiescent(wrong); err == nil || done {
		t.Fatal("foreign lease inherited completion proof")
	}
}

func TestSupervisorRecoveryCannotInventNativeProof(t *testing.T) {
	root := t.TempDir()
	s, err := openSupervisor(root)
	if err != nil {
		t.Fatal(err)
	}
	b := RunBinding{RunID: "run", LeaseID: "lease", Generation: 1, Identity: "uid-502"}
	if err = s.Launch(b, &exec.Cmd{}, &proofTree{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openSupervisor(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if done, err := s.Quiescent(b); err == nil || done {
		t.Fatal("restart invented missing native containment proof")
	}
	if err = s.Launch(b, &exec.Cmd{}, &proofTree{}); err == nil {
		t.Fatal("uncertain launch replayed on recovery")
	}
}
