package cachebrokerlab

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// StorageLayout is captured once by privileged disposable bootstrap and stored
// in its protected config. The service cannot discover/adopt replacement disks.
// Full filesystem limits, including retained unknown data and logs, are charged
// up front. These are capacity bounds, not a claim about unique APFS blocks.
type StorageLayout struct {
	BudgetBytes int64
	Control     string
	Volumes     []StorageVolume
	Slots       []StorageSlot
}

type StorageVolume struct {
	Root, Identity                          string
	CapacityBytes, LimitBytes, MinFreeBytes int64
}

type StorageSlot struct {
	Generation              uint64
	Artifact, Stage, Volume string
}

type storageReading struct {
	Identity            string
	Capacity, Available int64
}

type storageProbe func(string) (storageReading, error)

var errStorageExhausted = errors.New("aggregate storage budget exhausted; retained data will not be evicted")

type slotClaim struct {
	HomeID, RunID, LeaseID, State string
}

// CaptureStorage measures already provisioned fixed filesystems. It neither
// creates volumes nor erases data. Its result must be saved outside task homes.
func CaptureStorage(root string, layout StorageLayout) (*StorageLayout, error) {
	if err := layout.protected(root); err != nil {
		return nil, err
	}
	for i := range layout.Volumes {
		v := &layout.Volumes[i]
		r, err := nativeStorageVolume(v.Root)
		if err != nil {
			return nil, err
		}
		v.Identity, v.CapacityBytes = r.Identity, r.Capacity
	}
	if err := layout.check(root, true, nil); err != nil {
		return nil, err
	}
	return &layout, nil
}

func (l *StorageLayout) protected(root string) error {
	for _, p := range []string{root, l.Control} {
		if err := protectedPath(p); err != nil {
			return err
		}
	}
	for _, v := range l.Volumes {
		if err := protectedPath(v.Root); err != nil {
			return err
		}
	}
	for _, slot := range l.Slots {
		if err := protectedPath(filepath.Dir(slot.Stage)); err != nil {
			return err
		}
		st, err := os.Lstat(slot.Stage)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errors.New("registered slot is not a plain directory")
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

func (l *StorageLayout) check(root string, admission bool, probe storageProbe) error {
	if probe == nil {
		probe = newNativeStorageProbe()
	}
	if l.BudgetBytes <= 0 || l.BudgetBytes > 8<<30 || len(l.Volumes) < 2 || len(l.Volumes) > 8 || len(l.Slots) == 0 || len(l.Slots) > 128 || !filepath.IsAbs(root) || !filepath.IsAbs(l.Control) || within(root, l.Control) || within(l.Control, root) {
		return errors.New("invalid storage registry")
	}
	volumes := map[string]StorageVolume{}
	identities := map[string]bool{}
	var reserved int64
	stateVolume, controlVolume := "", ""
	for _, v := range l.Volumes {
		if !filepath.IsAbs(v.Root) || filepath.Clean(v.Root) != v.Root || v.Identity == "" || v.CapacityBytes <= 0 || v.CapacityBytes > v.LimitBytes || v.LimitBytes > 1<<30 || v.MinFreeBytes < 2*maxRegistryBytes || v.MinFreeBytes >= v.CapacityBytes || identities[v.Identity] || volumes[v.Root].Identity != "" {
			return errors.New("invalid or aliased storage volume")
		}
		for path := range volumes {
			if within(path, v.Root) || within(v.Root, path) {
				return errors.New("nested storage volumes are not supported")
			}
		}
		reserved += v.LimitBytes
		volumes[v.Root], identities[v.Identity] = v, true
		r, err := probe(v.Root)
		if err != nil {
			return fmt.Errorf("storage volume query: %w", err)
		}
		if r.Identity != v.Identity || r.Capacity != v.CapacityBytes || r.Available < 0 || r.Available > r.Capacity {
			return errors.New("storage volume identity/capacity changed; retain all data")
		}
		if admission && r.Available < v.MinFreeBytes {
			return errStorageExhausted
		}
		for path, selected := range map[string]*string{root: &stateVolume, l.Control: &controlVolume} {
			if within(v.Root, path) {
				if *selected != "" || path == v.Root {
					return errors.New("ambiguous service storage root")
				}
				*selected = v.Root
			}
		}
	}
	if reserved > l.BudgetBytes || stateVolume == "" || controlVolume != stateVolume {
		return errors.New("aggregate storage reservation exceeds budget or omits state/control")
	}
	slots := map[string]bool{}
	for i, slot := range l.Slots {
		v, ok := volumes[slot.Volume]
		key := fmt.Sprintf("%d/%s", slot.Generation, slot.Artifact)
		if !ok || slot.Volume == stateVolume || slot.Generation == 0 || slot.Generation > maxHomeGenerations || !exact(slot.Artifact) || !filepath.IsAbs(slot.Stage) || filepath.Clean(slot.Stage) != slot.Stage || slot.Stage == slot.Volume || !within(slot.Volume, slot.Stage) || slots[key] {
			return errors.New("invalid or duplicate storage slot")
		}
		for _, other := range l.Slots[:i] {
			if within(other.Stage, slot.Stage) || within(slot.Stage, other.Stage) {
				return errors.New("overlapping acquisition slots")
			}
		}
		slots[key] = true
		// Query each registered path, so a nested mount cannot escape its cap.
		r, err := probe(slot.Stage)
		if err != nil || r.Identity != v.Identity || r.Capacity != v.CapacityBytes {
			return errors.Join(errors.New("registered slot escaped its volume"), err)
		}
	}
	for _, path := range []string{root, l.Control} {
		r, err := probe(path)
		v := volumes[stateVolume]
		if err != nil || r.Identity != v.Identity || r.Capacity != v.CapacityBytes {
			return errors.Join(errors.New("service state/control escaped its volume"), err)
		}
	}
	return nil
}

func (s *Service) checkStorage(admission bool) error {
	if s.config.Storage == nil {
		return nil // Internal state-machine fixtures have no privileged storage.
	}
	if s.protectedStorage {
		if err := s.config.Storage.protected(s.config.Root); err != nil {
			return err
		}
	}
	return s.config.Storage.check(s.config.Root, admission, s.storageProbe)
}

func (s *Service) reserveSlot(h *Home, g *Generation, artifact string) (*StorageSlot, error) {
	if s.config.Storage == nil {
		return nil, nil
	}
	for _, slot := range s.config.Storage.Slots {
		if slot.Generation != g.ID || slot.Artifact != artifact {
			continue
		}
		if _, used := s.journal.Slots[slot.Stage]; used {
			return nil, errors.New("acquisition slot retained; reuse forbidden")
		}
		s.journal.Slots[slot.Stage] = slotClaim{HomeID: h.ID, RunID: g.RunID, LeaseID: g.LeaseID, State: "reserved"}
		if err := s.persist(); err != nil {
			return nil, err
		}
		if err := s.hit("storage.reserved"); err != nil {
			return nil, err
		}
		return &slot, nil
	}
	return nil, errors.New("no registered acquisition slot; data retained")
}

func (s *Service) validateStorageRegistry() error {
	if !reflect.DeepEqual(s.config.Storage, s.journal.Storage) {
		return errors.New("storage bootstrap differs from durable registry; no automatic migration")
	}
	if s.config.Storage == nil {
		if len(s.journal.Slots) != 0 {
			return errors.New("slot claims without storage registry")
		}
		return nil
	}
	if s.journal.Slots == nil || len(s.journal.Slots) > len(s.config.Storage.Slots) {
		return errors.New("invalid slot claim registry")
	}
	for path, claim := range s.journal.Slots {
		found := false
		for _, slot := range s.config.Storage.Slots {
			if slot.Stage != path {
				continue
			}
			h := s.journal.Homes[claim.HomeID]
			if h == nil || h.Generations[slot.Generation] == nil {
				return errors.New("slot has no owning generation")
			}
			g := h.Generations[slot.Generation]
			if g.RunID != claim.RunID || g.LeaseID != claim.LeaseID || g.Artifacts[slot.Artifact] == "" || (claim.State != "reserved" && claim.State != "retained" && claim.State != "quarantined") {
				return errors.New("slot claim binding or state is invalid")
			}
			found = true
		}
		if !found {
			return errors.New("unregistered slot claim")
		}
	}
	return nil
}
