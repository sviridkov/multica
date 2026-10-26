//go:build cachebrokerlab

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
)

type acquisitionIdentity struct {
	UID, GID            uint32
	User, Password, SID string
}

// These finite slots are installed by the disposable privileged bootstrap.
// Neither their identity nor their filesystem can be selected through IPC.
type acquisitionSlot struct {
	Generation              uint64
	Artifact, Stage, Volume string
	Identity                acquisitionIdentity
}

type acquisitionDomain interface {
	cachebrokerlab.ProcessDomain
	Close() error
}

func acquisitionFactory(cfg config) cachebrokerlab.ProductionFactory {
	if len(cfg.Acquisitions) == 0 {
		return nil
	}
	return func(homeID, artifact string, binding cachebrokerlab.RunBinding) (cachebrokerlab.ProductionPlan, func() error, error) {
		var selected *acquisitionSlot
		for i := range cfg.Acquisitions {
			slot := &cfg.Acquisitions[i]
			if slot.Generation == binding.Generation && slot.Artifact == artifact {
				if selected != nil {
					return cachebrokerlab.ProductionPlan{}, nil, errors.New("ambiguous acquisition slot")
				}
				selected = slot
			}
		}
		if selected == nil {
			return cachebrokerlab.ProductionPlan{}, nil, errors.New("no provisioned acquisition slot; retain generation")
		}
		slot := *selected
		kind := "catalog"
		if artifact == cachebrokerlab.Marketplaces {
			kind = "marketplace"
		}
		journal := filepath.Join(cfg.Control, fmt.Sprintf("acquisition-%s-%d-%s", homeID, binding.Generation, kind))
		if err := os.Mkdir(journal, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return cachebrokerlab.ProductionPlan{}, nil, err
		}
		supervisor, err := cachebrokerlab.OpenSupervisor(journal)
		if err != nil {
			return cachebrokerlab.ProductionPlan{}, nil, err
		}
		domain, identity, err := newAcquisitionDomain(cfg, slot, filepath.Base(journal))
		if err != nil {
			return cachebrokerlab.ProductionPlan{}, nil, errors.Join(err, supervisor.Close())
		}
		binding.Identity = identity
		plan := cachebrokerlab.ProductionPlan{Stage: slot.Stage, Volume: slot.Volume, CapacityBytes: 128 << 20, LogicalBytes: 8 << 20, MaxEntries: 2048, Binding: binding, Supervisor: supervisor, Domain: domain}
		return plan, func() error { return errors.Join(domain.Close(), supervisor.Close()) }, nil
	}
}

// The launcher runs only after the OS has dropped identity and established the
// native domain. Its directories and the measured producer inherit that domain.
func runAcquisition(producer string) error {
	identity, err := ordinaryAcquisitionIdentity()
	if err != nil {
		return err
	}
	for _, rel := range []string{"tmp", "no-hooks", "codex-home"} {
		if err := os.Mkdir(rel, 0700); err != nil {
			return err
		}
	}
	// Retention sentinel belongs to this disposable acquisition fixture. It is
	// outside both exact cache paths and must never be copied into publication.
	if err := os.WriteFile("unknown.txt", []byte("unclassified staging bytes"), 0600); err != nil {
		return err
	}
	fmt.Printf("SERVICE_PRODUCER ordinary=true identity=%s\n", identity)
	cmd := exec.Command(producer, "--exact", "remote::broker_lab::produce", "--ignored", "--nocapture")
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func acquisitionCommand(cmd *exec.Cmd) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	producer := cmd.Path
	cmd.Path, cmd.Args = self, []string{self, "--acquire", producer}
	return nil
}

func immutableProfile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if errors.Is(err, os.ErrExist) {
		previous, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(previous, data) {
			return errors.New("existing acquisition profile differs; retain it")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Sync(), f.Close())
}
