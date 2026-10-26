package cachebrokerlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type RunBinding struct {
	RunID, LeaseID, Identity string
	Generation               uint64
}

// ProcessDomain must enforce containment before task code executes. An active
// count covers every descendant, including detached/reparented processes.
type ProcessDomain interface {
	Start(*exec.Cmd) error
	Active() (uint32, error)
}
type supervisedRun struct {
	Binding RunBinding
	Sealed  bool
}
type supervisorJournal struct {
	Version int
	Runs    map[string]supervisedRun
}

// Supervisor owns launch authority separately from the restartable runtime
// driver and cache service. A recovered journal never recreates kernel proof.
type Supervisor struct {
	mu       sync.Mutex
	root     string
	lock     *os.File
	journal  supervisorJournal
	domains  map[string]ProcessDomain
	poisoned bool
}

func OpenSupervisor(root string) (*Supervisor, error) {
	if err := privateCredential(root); err != nil {
		return nil, err
	}
	return openSupervisor(root)
}
func openSupervisor(root string) (*Supervisor, error) {
	lock, err := lockService(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		return nil, err
	}
	s := &Supervisor{root: root, lock: lock, domains: map[string]ProcessDomain{}, journal: supervisorJournal{Version: 1, Runs: map[string]supervisedRun{}}}
	fail := func(err error) (*Supervisor, error) { s.Close(); return nil, err }
	f, err := os.Open(filepath.Join(root, "runs.json"))
	if errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(root)
		if e != nil {
			return fail(e)
		}
		if len(entries) != 1 || entries[0].Name() != "supervisor.lock" {
			return fail(errors.New("missing supervisor journal in nonempty namespace"))
		}
		if err = s.persist(); err != nil {
			return fail(err)
		}
		return s, nil
	}
	if err != nil {
		return fail(err)
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	err = errors.Join(err, f.Close())
	if err != nil {
		return fail(err)
	}
	if len(data) > 1<<20 {
		return fail(errors.New("supervisor journal size exceeded"))
	}
	if err = strictJSON(data, &s.journal); err != nil {
		return fail(err)
	}
	if s.journal.Version != 1 || s.journal.Runs == nil || len(s.journal.Runs) > 64 {
		return fail(errors.New("unsupported supervisor journal"))
	}
	identities := map[string]bool{}
	for id, r := range s.journal.Runs {
		if id != r.Binding.RunID || !validRunBinding(r.Binding) || identities[r.Binding.Identity] {
			return fail(errors.New("invalid supervisor binding"))
		}
		identities[r.Binding.Identity] = true
	}
	return s, nil
}
func validRunBinding(b RunBinding) bool {
	return b.RunID != "" && len(b.RunID) <= 128 && b.LeaseID != "" && len(b.LeaseID) <= 128 && b.Generation > 0 && b.Identity != "" && len(b.Identity) <= 128
}
func (s *Supervisor) persist() (err error) {
	defer func() {
		if err != nil {
			s.poisoned = true
		}
	}()
	data, err := json.Marshal(s.journal)
	if err != nil {
		return err
	}
	name := filepath.Join(s.root, "runs-"+nonce()+".tmp")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return replaceServiceFile(name, filepath.Join(s.root, "runs.json"))
}
func (s *Supervisor) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// Launch consumes the identity and run before any process exists. A launch
// error stays uncertain; it must never be retried as a new process.
func (s *Supervisor) Launch(b RunBinding, cmd *exec.Cmd, domain ProcessDomain) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil || s.poisoned {
		return errors.New("supervisor unavailable")
	}
	if !validRunBinding(b) || cmd == nil || domain == nil {
		return errors.New("invalid supervised launch")
	}
	if _, ok := s.journal.Runs[b.RunID]; ok {
		return errors.New("run already reserved; replay forbidden")
	}
	if len(s.journal.Runs) >= 64 {
		return errors.New("supervisor capacity reached; preserve retained runs")
	}
	for _, r := range s.journal.Runs {
		if r.Binding.Identity == b.Identity {
			return errors.New("task identity reuse forbidden")
		}
	}
	s.journal.Runs[b.RunID] = supervisedRun{Binding: b}
	if err := s.persist(); err != nil {
		return err
	}
	if err := domain.Start(cmd); err != nil {
		return fmt.Errorf("uncertain supervised launch: %w", err)
	}
	s.domains[b.RunID] = domain
	return nil
}

// Quiescent seals the sole launch authority before querying the kernel domain.
// Driver completion, a disconnected pipe, or a missing process is not proof.
func (s *Supervisor) Quiescent(b RunBinding) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil || s.poisoned {
		return false, errors.New("supervisor unavailable")
	}
	r, ok := s.journal.Runs[b.RunID]
	if !ok || r.Binding != b {
		return false, errors.New("unknown run or mismatched lease")
	}
	if !r.Sealed {
		r.Sealed = true
		s.journal.Runs[b.RunID] = r
		if err := s.persist(); err != nil {
			return false, err
		}
	}
	domain := s.domains[b.RunID]
	if domain == nil {
		return false, errors.New("native containment proof unavailable after uncertain launch/recovery")
	}
	n, err := domain.Active()
	if err != nil {
		return false, err
	}
	return n == 0, nil
}
