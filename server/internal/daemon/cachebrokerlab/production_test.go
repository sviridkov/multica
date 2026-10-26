package cachebrokerlab

import (
	"errors"
	"os/exec"
	"testing"
)

func TestProductionRequiresSuccessfulExitAndWholeTreeProof(t *testing.T) {
	s, err := openSupervisor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := RunBinding{RunID: "producer", LeaseID: "lease", Identity: "ordinary", Generation: 1}
	d := &proofTree{active: 1}
	if err = s.Launch(b, &exec.Cmd{}, d); err != nil {
		t.Fatal(err)
	}
	p := &Production{supervisor: s, binding: b}
	if p.ready() == nil {
		t.Error("unfinished leader authorized publication")
	}
	p.waited = true
	p.waitErr = errors.New("producer quota exhausted")
	if p.ready() == nil {
		t.Error("failed producer authorized publication")
	}
	p.waitErr = nil
	if p.ready() == nil {
		t.Error("detached writer authorized publication")
	}
	d.active = 0
	d.err = errors.New("native query failed")
	if p.ready() == nil {
		t.Error("missing native proof authorized publication")
	}
	d.err = nil
	if err = p.ready(); err != nil {
		t.Fatal(err)
	}
	p.binding.Generation++
	if p.ready() == nil {
		t.Error("foreign generation inherited proof")
	}
}

func TestProductionDoesNotRecoverProofFromJournal(t *testing.T) {
	root := t.TempDir()
	s, err := openSupervisor(root)
	if err != nil {
		t.Fatal(err)
	}
	b := RunBinding{RunID: "producer", LeaseID: "lease", Identity: "ordinary", Generation: 1}
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
	p := &Production{supervisor: s, binding: b, waited: true}
	if p.ready() == nil {
		t.Fatal("recovered journal authorized publication without native proof")
	}
}
