package cachebrokerlab

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// ProductionPlan is provisioned by the privileged test bootstrap, never by a
// task. Volume is a separately mounted, capacity-limited staging filesystem.
// Unknown and failed stages stay on that volume and consume its finite space.
type ProductionPlan struct {
	Stage, Volume               string
	CapacityBytes, LogicalBytes int64
	MaxEntries                  int
	Binding                     RunBinding
	Supervisor                  *Supervisor
	Domain                      ProcessDomain
}

type volumeIdentity struct {
	ID       string
	Capacity int64
}

// Production holds a single live supervisor capability. It cannot be restored
// from a receipt or a recovered journal after native containment is lost.
type Production struct {
	mu         sync.Mutex
	supervisor *Supervisor
	binding    RunBinding
	waited     bool
	waitErr    error
	broker     *Broker
	plan       ProductionPlan
	volume     volumeIdentity
	receipt    Receipt
	cmd        *exec.Cmd
	consumed   bool
}

func (p *Production) ready() error {
	if !p.waited || p.waitErr != nil {
		return errors.New("producer has not exited successfully")
	}
	if p.supervisor == nil {
		return errors.New("missing producer supervisor")
	}
	ok, err := p.supervisor.Quiescent(p.binding)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("producer descendants remain active")
	}
	return nil
}

func (p ProductionPlan) verifyVolume(destination string) (volumeIdentity, error) {
	if !filepath.IsAbs(p.Stage) || !filepath.IsAbs(p.Volume) || p.CapacityBytes <= 0 || p.CapacityBytes > 1<<30 || p.LogicalBytes <= 0 || p.LogicalBytes > p.CapacityBytes || p.MaxEntries <= 0 || p.MaxEntries > 4096 {
		return volumeIdentity{}, errors.New("invalid bounded staging policy")
	}
	rel, err := filepath.Rel(p.Volume, p.Stage)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return volumeIdentity{}, errors.New("stage is outside registered volume")
	}
	for path := p.Stage; ; path = filepath.Dir(path) {
		st, err := os.Lstat(path)
		if err != nil {
			return volumeIdentity{}, err
		}
		if !st.IsDir() || st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return volumeIdentity{}, errors.New("staging path is not a plain directory")
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	volume, err := nativeVolume(p.Volume)
	if err != nil {
		return volumeIdentity{}, err
	}
	stage, err := nativeVolume(p.Stage)
	if err != nil {
		return volumeIdentity{}, err
	}
	dest, err := nativeVolume(destination)
	if err != nil {
		return volumeIdentity{}, err
	}
	if volume.ID != stage.ID || volume.ID == dest.ID || volume.Capacity <= 0 || volume.Capacity > p.CapacityBytes || stage.Capacity != volume.Capacity {
		return volumeIdentity{}, errors.New("staging capacity or filesystem isolation not established")
	}
	return volume, nil
}

// StartProduction reserves an unproven generation before launching the measured
// producer under a distinct native identity. The final cache is never writable
// by that identity. Service prepare uses the same capability and publication.
func (b *Broker) StartProduction(ctx context.Context, artifact string, plan ProductionPlan) (*Production, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !exact(artifact) || plan.Supervisor == nil || plan.Domain == nil || !validRunBinding(plan.Binding) {
		return nil, errors.New("invalid supervised production")
	}
	if _, err := b.root.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("existing artifact retained")
	}
	old := b.records[artifact]
	if old.State != "" && old.State != "consumed" {
		return nil, errors.New("unresolved production retained")
	}
	generation := old.Generation + 1
	if b.policy.Generation != 0 {
		generation = b.policy.Generation
		if old.State != "" {
			return nil, errors.New("service generation cannot repeat acquisition")
		}
	}
	if plan.Binding.Generation != generation {
		return nil, errors.New("producer generation mismatch")
	}
	if err := protectedPath(b.policy.Root); err != nil {
		return nil, err
	}
	if err := protectedPath(filepath.Dir(plan.Stage)); err != nil {
		return nil, err
	}
	if err := verifyBinary(b.policy.Producer, b.policy.ProducerSHA256); err != nil {
		return nil, err
	}
	volume, err := plan.verifyVolume(b.policy.Root)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(plan.Stage)
	if err != nil {
		return nil, err
	}
	if err = nativeStageEntry(plan.Stage, st, plan.Binding.Identity); err != nil {
		return nil, err
	}
	contents, err := os.ReadDir(plan.Stage)
	if err != nil || len(contents) != 0 {
		return nil, errors.New("staging must be fresh; unknown data retained")
	}
	retained, err := filepath.Glob(filepath.Join(b.policy.Root, ".publication-*"))
	if err != nil || len(retained) >= 16 {
		return nil, errors.New("publication quarantine capacity reached")
	}
	r := Receipt{Schema: 1, HomeID: b.policy.HomeID, Artifact: artifact, ProducerSHA256: b.policy.ProducerSHA256, SourceOID: b.policy.SourceOID, Generation: generation, State: "unproven"}
	if artifact == Catalog {
		r.SourceOID = ""
	}
	b.records[artifact] = r
	if err = b.persist(); err != nil {
		return nil, err
	}
	if err = b.hit("production.intent"); err != nil {
		return nil, err
	}
	kind := "catalog"
	if artifact == Marketplaces {
		kind = "marketplace"
	}
	recipe := b.policy
	recipe.Root = plan.Stage
	cmd := exec.CommandContext(ctx, b.policy.Producer, "--exact", "remote::broker_lab::produce", "--ignored", "--nocapture")
	cmd.Dir = plan.Stage
	cmd.Env = append(producerEnvironment(recipe, kind), "MULTICA_CACHE_LAB_EVIDENCE="+filepath.Join(plan.Stage, "evidence.json"))
	cmd.WaitDelay = 5 * time.Second
	// Producer output is diagnostic only; it cannot grow the privileged journal.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	p := &Production{supervisor: plan.Supervisor, binding: plan.Binding, broker: b, plan: plan, volume: volume, receipt: r, cmd: cmd}
	if err = plan.Supervisor.Launch(plan.Binding, cmd, plan.Domain); err != nil {
		return nil, err
	}
	if err = b.hit("production.running"); err != nil {
		return nil, errors.Join(err, cmd.Process.Kill(), cmd.Wait())
	}
	return p, nil
}

func (p *Production) Wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.waited {
		p.waitErr = p.cmd.Wait()
		p.waited = true
	}
	return p.waitErr
}

// Publish copies only a fully completed acquisition into a new protected tree.
// No failure removes a stage, replaces unknown content, or grants a GC receipt.
func (p *Production) Publish(ctx context.Context) (Receipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.consumed {
		return Receipt{}, errors.New("publication capability already consumed")
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := p.ready(); err != nil {
		return Receipt{}, err
	}
	if err := b.hit("production.quiescent"); err != nil {
		return Receipt{}, err
	}
	if b.lease == nil || !reflect.DeepEqual(b.records[p.receipt.Artifact], p.receipt) {
		return Receipt{}, errors.New("production intent changed")
	}
	volume, err := p.plan.verifyVolume(b.policy.Root)
	if err != nil {
		return Receipt{}, err
	}
	if volume != p.volume {
		return Receipt{}, errors.New("staging volume changed")
	}
	if err = validateStage(p.plan); err != nil {
		return Receipt{}, err
	}
	data, err := readBoundedJournal(filepath.Join(p.plan.Stage, "evidence.json"), 16<<10)
	if err != nil {
		return Receipt{}, err
	}
	r := p.receipt
	if err = strictJSON(data, &r.Source); err != nil {
		return Receipt{}, err
	}
	if err = validateSource(b.policy, r.Artifact, r.Source); err != nil {
		return Receipt{}, err
	}
	if _, err = b.root.Lstat(r.Artifact); !errors.Is(err, os.ErrNotExist) {
		return Receipt{}, errors.New("unknown destination retained")
	}
	parent := filepath.Dir(filepath.Join(b.policy.Root, filepath.FromSlash(r.Artifact)))
	if err = os.MkdirAll(parent, 0755); err != nil {
		return Receipt{}, err
	}
	if err = protectedArtifactPath(parent, b.policy); err != nil {
		return Receipt{}, err
	}
	// From here every attempt is one-shot: ambiguous copying, rename or journal
	// failure retains the reservation and any protected partial publication.
	p.consumed = true
	name := ".publication-" + nonce()
	if err = b.root.Mkdir(name, 0755); err != nil {
		return Receipt{}, err
	}
	dest := filepath.Join(b.policy.Root, name)
	source := filepath.Join(p.plan.Stage, filepath.FromSlash(r.Artifact))
	if err = copyStage(ctx, source, dest, p.plan); err != nil {
		return Receipt{}, err
	}
	if err = b.hit("publication.after-copy"); err != nil {
		return Receipt{}, err
	}
	if err = publishReadable(dest); err != nil {
		return Receipt{}, err
	}
	if err = protectedPath(dest); err != nil {
		return Receipt{}, err
	}
	if err = renameNewTree(dest, filepath.Join(b.policy.Root, filepath.FromSlash(r.Artifact))); err != nil {
		return Receipt{}, err
	}
	if err = b.hit("publication.after-rename"); err != nil {
		return Receipt{}, err
	}
	r.Entries, err = inventory(filepath.Join(b.policy.Root, filepath.FromSlash(r.Artifact)))
	if err != nil {
		return Receipt{}, err
	}
	r.State = "committed"
	if err = b.hit("publication.before-commit"); err != nil {
		return Receipt{}, err
	}
	b.records[r.Artifact] = r
	if err = b.persist(); err != nil {
		b.records[r.Artifact] = p.receipt
		return Receipt{}, err
	}
	if err = b.hit("publication.after-commit"); err != nil {
		return Receipt{}, err
	}
	return cloneReceipt(r), nil
}

func validateStage(p ProductionPlan) error {
	var bytes int64
	count := 0
	return filepath.WalkDir(p.Stage, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.IsDir() && !st.Mode().IsRegular() {
			return errors.New("unsupported staging entry")
		}
		if err = nativeStageEntry(path, st, p.Binding.Identity); err != nil {
			return err
		}
		count++
		if count > p.MaxEntries {
			return errors.New("staging entry limit exceeded")
		}
		if st.Mode().IsRegular() {
			if st.Size() > p.LogicalBytes-bytes {
				return errors.New("staging logical quota exceeded")
			}
			bytes += st.Size()
		}
		return nil
	})
}

func copyStage(ctx context.Context, source, dest string, p ProductionPlan) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	var bytes int64
	count := 0
	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if err = nativeStageEntry(path, st, p.Binding.Identity); err != nil {
			return err
		}
		count++
		if count > p.MaxEntries {
			return errors.New("publication entry limit exceeded")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if st.IsDir() {
			if rel == "." {
				return nil
			}
			return os.Mkdir(filepath.Join(dest, rel), 0755)
		}
		if !st.Mode().IsRegular() || st.Size() > p.LogicalBytes-bytes {
			return errors.New("unsupported or oversized publication entry")
		}
		in, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer in.Close()
		opened, err := in.Stat()
		if err != nil || !os.SameFile(st, opened) {
			return errors.New("staging entry replaced")
		}
		out, err := os.OpenFile(filepath.Join(dest, rel), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644|(st.Mode().Perm()&0111))
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, p.LogicalBytes-bytes+1))
		err = errors.Join(copyErr, out.Sync(), out.Close())
		if err != nil {
			return err
		}
		if n != st.Size() || n > p.LogicalBytes-bytes {
			return errors.New("publication changed or exceeded quota")
		}
		bytes += n
		return nil
	})
}
