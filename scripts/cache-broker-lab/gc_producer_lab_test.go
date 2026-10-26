//go:build cachebrokerlab && (darwin || windows)

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
)

type producerConfig struct {
	Recipe                                                 cachebrokerlab.Policy
	Stage, Volume, Journal, Control, Artifact, Mode, Route string
	Identity                                               runtimeIdentity
	Binding                                                cachebrokerlab.RunBinding
}
type producerReply struct {
	Error   string
	Receipt cachebrokerlab.Receipt
	Removed int
	Bytes   int64
}
type producerProcess struct {
	cfg      producerConfig
	path     string
	cmd      *exec.Cmd
	in       io.WriteCloser
	sequence int
}

func producerRecipe(t *testing.T, cfg nativeLab) (string, cachebrokerlab.Policy) {
	t.Helper()
	var recipe cachebrokerlab.Policy
	root, b := labBroker(t, cfg, func(p *cachebrokerlab.Policy) { recipe = *p })
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	labWrite(t, filepath.Join(root, "workspace", "result.txt"), "task result outside staging quota")
	return root, recipe
}

func startProducer(t *testing.T, cfg nativeLab, recipe cachebrokerlab.Policy, volume, artifact, route, mode string, generation uint64) *producerProcess {
	t.Helper()
	id := allocateRuntimeIdentity(t, cfg)
	control, err := os.MkdirTemp(cfg.Root, "producer-control-")
	if err != nil {
		t.Fatal(err)
	}
	nativePrivate(t, control)
	journal := filepath.Join(control, "supervisor")
	if err = os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	nativePrivate(t, journal)
	stage := nativeProducerStage(t, volume, id)
	p := &producerProcess{cfg: producerConfig{Recipe: recipe, Stage: stage, Volume: volume, Journal: journal, Control: control, Artifact: artifact, Route: route, Mode: mode, Identity: id, Binding: cachebrokerlab.RunBinding{RunID: filepath.Base(control), LeaseID: "producer-acquisition", Identity: nativeRuntimeIdentity(id), Generation: generation}}}
	data, err := json.Marshal(p.cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.path = filepath.Join(control, "config.json")
	if err = os.WriteFile(p.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command(exe, "-test.run=^TestBrokerProducerSupervisor$")
	p.cmd.Env = append(nativeBaseEnv(), "MULTICA_NATIVE_BROKER_CONFIG="+os.Getenv("MULTICA_NATIVE_BROKER_CONFIG"), "MULTICA_PRODUCER_CONFIG="+p.path)
	p.cmd.Stdout, p.cmd.Stderr = os.Stdout, os.Stderr
	p.in, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.in.Close()
		if p.cmd.ProcessState == nil {
			p.cmd.Process.Kill()
			p.cmd.Wait()
		}
	})
	waitRuntimeFile(t, filepath.Join(control, "leader-finished"))
	return p
}
func (p *producerProcess) request(t *testing.T, operation string) producerReply {
	t.Helper()
	p.sequence++
	if _, err := fmt.Fprintln(p.in, operation); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.cfg.Control, fmt.Sprintf("reply-%d.json", p.sequence))
	waitRuntimeFile(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r producerReply
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func (p *producerProcess) close(t *testing.T) {
	t.Helper()
	p.in.Close()
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
func (p *producerProcess) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.cfg.Stage, "release"), []byte("release"), 0644); err != nil {
		t.Fatal(err)
	}
	nativeProducerReleaseOwner(t, filepath.Join(p.cfg.Stage, "release"), p.cfg.Identity)
}
func producerPreserved(t *testing.T, root, stage, other string) {
	t.Helper()
	for path, want := range map[string]string{filepath.Join(root, "workspace", "result.txt"): "task result outside staging quota", filepath.Join(stage, "unknown.txt"): "unclassified staging bytes", filepath.Join(root, filepath.FromSlash(other), "unknown.txt"): "unknown other cache"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("preservation failed %s: %v", path, err)
		}
	}
}

func TestBrokerNativeIsolatedProducerCycles(t *testing.T) {
	cfg := mustLab(t)
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				volume := nativeProducerVolume(t, cfg)
				root, recipe := producerRecipe(t, cfg)
				other := cachebrokerlab.Catalog
				if artifact == other {
					other = cachebrokerlab.Marketplaces
				}
				labWrite(t, filepath.Join(root, filepath.FromSlash(other), "unknown.txt"), "unknown other cache")
				for generation := uint64(1); generation <= 2; generation++ {
					p := startProducer(t, cfg, recipe, volume, artifact, route, "held", generation)
					waitRuntimeFile(t, filepath.Join(p.cfg.Stage, "descendant-ready"))
					if r := p.request(t, "publish"); r.Error == "" || r.Receipt.State == "committed" {
						t.Fatal("live descendant received receipt", r)
					}
					if r := p.request(t, "gc"); r.Removed != 0 || r.Bytes != 0 {
						t.Fatal("active production reclaimed", r)
					}
					p.release(t)
					if r := p.request(t, "quiesce"); r.Error != "" {
						t.Fatal(r.Error)
					}
					if r := p.request(t, "failed-query"); r.Error == "" || r.Receipt.State == "committed" {
						t.Fatal("failed native query authorized publication", r)
					}
					r := p.request(t, "publish")
					if r.Error != "" || r.Receipt.State != "committed" || r.Receipt.Generation != generation {
						t.Fatal("publish failed", r)
					}
					var want int64
					for _, e := range r.Receipt.Entries {
						want += e.Bytes
					}
					if want <= 0 {
						t.Fatal("no bytes acquired")
					}
					// An ordinary producer identity has no write access to the imported tree.
					attack := nativeAttackCommand(t, cfg, root, "producer-publication")
					if out, err := attack.CombinedOutput(); err != nil {
						t.Fatalf("publication not protected: %v %s", err, out)
					}
					if replay := p.request(t, "publish"); replay.Error == "" {
						t.Fatal("publication capability replayed")
					}
					gc := p.request(t, "gc")
					if gc.Error != "" || gc.Removed != 1 || gc.Bytes != want {
						t.Fatal("positive GC failed", gc)
					}
					producerPreserved(t, root, p.cfg.Stage, other)
					p.close(t)
					t.Logf("ISOLATED_PRODUCER_CYCLE route=%s artifact=%s generation=%d logical_bytes=%d volume_capacity_limit=%d staging_retained=true", route, artifact, generation, want, 128<<20)
				}
			})
		}
	}
}

func TestBrokerNativeIsolatedProducerFailures(t *testing.T) {
	cfg := mustLab(t)
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				volume := nativeProducerVolume(t, cfg)
				for _, mode := range []string{"partial", "logical-quota", "symlink", "hardlink", "physical-quota"} {
					t.Run(mode, func(t *testing.T) {
						root, recipe := producerRecipe(t, cfg)
						other := cachebrokerlab.Catalog
						if artifact == other {
							other = cachebrokerlab.Marketplaces
						}
						labWrite(t, filepath.Join(root, filepath.FromSlash(other), "unknown.txt"), "unknown other cache")
						p := startProducer(t, cfg, recipe, volume, artifact, route, mode, 1)
						if r := p.request(t, "publish"); r.Error == "" || r.Receipt.State == "committed" {
							t.Fatal("unsafe production published", r)
						}
						if r := p.request(t, "gc"); r.Removed != 0 || r.Bytes != 0 {
							t.Fatal("failed acquisition reclaimed", r)
						}
						if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifact))); !os.IsNotExist(err) {
							t.Fatal("unproven destination appeared", err)
						}
						producerPreserved(t, root, p.cfg.Stage, other)
						if mode == "physical-quota" {
							data, err := os.ReadFile(filepath.Join(p.cfg.Control, "leader-finished"))
							if err != nil || !strings.Contains(string(data), "exit status 27") {
								t.Fatal("quota did not fail producer", string(data), err)
							}
						}
						p.close(t)
						t.Logf("ISOLATED_PRODUCER_DENIED mode=%s route=%s artifact=%s receipt=false byte_credit=0 unknown_preserved=true", mode, route, artifact)
					})
				}
			})
		}
	}
}

func TestBrokerNativeProducerSupervisorCrash(t *testing.T) {
	cfg := mustLab(t)
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				volume := nativeProducerVolume(t, cfg)
				root, recipe := producerRecipe(t, cfg)
				other := cachebrokerlab.Catalog
				if artifact == other {
					other = cachebrokerlab.Marketplaces
				}
				labWrite(t, filepath.Join(root, filepath.FromSlash(other), "unknown.txt"), "unknown other cache")
				p := startProducer(t, cfg, recipe, volume, artifact, route, "held", 1)
				waitRuntimeFile(t, filepath.Join(p.cfg.Stage, "descendant-ready"))
				if r := p.request(t, "publish"); r.Error == "" {
					t.Fatal("live descendant published")
				}
				watch := nativeProducerCrashWatch(t, cfg, p.cfg)
				if err := p.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err := p.cmd.Wait(); err == nil {
					t.Fatal("supervisor did not crash")
				}
				p.in.Close()
				watch()
				p.release(t)
				nativeProducerDrain(t, cfg, p.cfg)
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				recovered := exec.Command(exe, "-test.run=^TestBrokerProducerRecovery$")
				recovered.Env = append(nativeBaseEnv(), "MULTICA_PRODUCER_CONFIG="+p.path)
				if out, err := recovered.CombinedOutput(); err != nil {
					t.Fatalf("recovery safety failed: %v %s", err, out)
				}
				producerPreserved(t, root, p.cfg.Stage, other)
				t.Logf("PRODUCER_SUPERVISOR_CRASH route=%s artifact=%s supervisor_pid=%d actual_process_killed=true native_proof_lost=true replay_denied=true receipt=false stage_preserved=true", route, artifact, p.cmd.Process.Pid)
			})
		}
	}
}

func producerReadConfig(t *testing.T) producerConfig {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("MULTICA_PRODUCER_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var c producerConfig
	if err = json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestBrokerProducerSupervisor(t *testing.T) {
	if os.Getenv("MULTICA_PRODUCER_CONFIG") == "" {
		return
	}
	c := producerReadConfig(t)
	cfg := mustLab(t)
	b, err := cachebrokerlab.Open(c.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	s, err := cachebrokerlab.OpenSupervisor(c.Journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := newNativeProducerDomain(t, cfg, c)
	p, err := b.StartProduction(context.Background(), c.Artifact, cachebrokerlab.ProductionPlan{Stage: c.Stage, Volume: c.Volume, CapacityBytes: 128 << 20, LogicalBytes: 8 << 20, MaxEntries: 2048, Binding: c.Binding, Supervisor: s, Domain: d})
	if err != nil {
		t.Fatal(err)
	}
	state := "producer success"
	if err = p.Wait(); err != nil {
		state = "producer failed: " + err.Error()
	}
	labWrite(t, filepath.Join(c.Control, "leader-finished"), state)
	scanner := bufio.NewScanner(os.Stdin)
	i := 0
	for scanner.Scan() {
		i++
		r := producerReply{}
		switch scanner.Text() {
		case "failed-query":
			restore := nativeRuntimeFailQuery(d.inner)
			r.Receipt, err = p.Publish(context.Background())
			restore()
		case "publish":
			r.Receipt, err = p.Publish(context.Background())
		case "gc":
			r.Removed, r.Bytes = labReclaim(t, c.Recipe.Root, c.Route, b)
			err = nil
		case "quiesce":
			deadline := time.Now().Add(20 * time.Second)
			for {
				var ok bool
				ok, err = s.Quiescent(c.Binding)
				if err != nil || ok {
					break
				}
				if time.Now().After(deadline) {
					err = fmt.Errorf("descendants did not quiesce")
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
		default:
			err = fmt.Errorf("unknown supervisor command")
		}
		if err != nil {
			r.Error = err.Error()
		}
		data, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(c.Control, fmt.Sprintf("reply-%d.json", i))
		if e = os.WriteFile(path+".tmp", data, 0600); e != nil {
			t.Fatal(e)
		}
		if e = os.Rename(path+".tmp", path); e != nil {
			t.Fatal(e)
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerProducerRecovery(t *testing.T) {
	if os.Getenv("MULTICA_PRODUCER_CONFIG") == "" {
		return
	}
	c := producerReadConfig(t)
	s, err := cachebrokerlab.OpenSupervisor(c.Journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ok, err := s.Quiescent(c.Binding); err == nil || ok {
		t.Fatal("restart manufactured native proof")
	}
	if err = s.Launch(c.Binding, &exec.Cmd{}, &producerNeverDomain{}); err == nil {
		t.Fatal("restart replayed producer")
	}
	if b, err := cachebrokerlab.Open(c.Recipe); err == nil {
		b.Close()
		t.Fatal("uncertain broker lease silently adopted")
	}
	data, err := os.ReadFile(filepath.Join(c.Recipe.Root, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]cachebrokerlab.Receipt
	if err = json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	if records[c.Artifact].State != "unproven" {
		t.Fatal("crashed producer left reclaimable receipt")
	}
	if _, err = os.Stat(filepath.Join(c.Recipe.Root, filepath.FromSlash(c.Artifact))); !os.IsNotExist(err) {
		t.Fatal("crash published artifact", err)
	}
}

type producerNeverDomain struct{}

func (*producerNeverDomain) Start(*exec.Cmd) error   { return fmt.Errorf("must not launch") }
func (*producerNeverDomain) Active() (uint32, error) { return 0, fmt.Errorf("no native proof") }

type nativeProducerDomain struct {
	inner  *nativeRuntimeDomain
	config producerConfig
	exe    string
}

func (d *nativeProducerDomain) Start(cmd *exec.Cmd) error {
	// The measured trampoline only adds failure/descendant probes around the
	// real pinned producer, under the same inherited identity and containment.
	cmd.Env = append(cmd.Env, "MULTICA_PRODUCER_BINARY="+cmd.Path, "MULTICA_PRODUCER_STAGE="+d.config.Stage, "MULTICA_PRODUCER_DEST="+d.config.Recipe.Root, "MULTICA_PRODUCER_CONTROL="+d.config.Control, "MULTICA_PRODUCER_MODE="+d.config.Mode)
	cmd.Path = d.exe
	cmd.Args = []string{d.exe, "-test.run=^TestBrokerProducerWorker$"}
	return d.inner.Start(cmd)
}
func (d *nativeProducerDomain) Active() (uint32, error) { return d.inner.Active() }

func TestBrokerProducerWorker(t *testing.T) {
	stage := os.Getenv("MULTICA_PRODUCER_STAGE")
	if stage == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("producer remains privileged")
	}
	for _, rel := range []string{"tmp", "no-hooks", "codex-home"} {
		if err := os.Mkdir(filepath.Join(stage, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	labWrite(t, filepath.Join(stage, "unknown.txt"), "unclassified staging bytes")
	if err := os.WriteFile(filepath.Join(os.Getenv("MULTICA_PRODUCER_DEST"), "producer-tamper"), []byte("unsafe"), 0600); err == nil {
		t.Fatal("producer can modify privileged publication")
	}
	if _, err := os.ReadFile(filepath.Join(os.Getenv("MULTICA_PRODUCER_CONTROL"), "config.json")); err == nil {
		t.Fatal("producer can read supervisor credentials")
	}
	t.Logf("PRODUCER_IDENTITY ordinary=true identity=%s", nativeIdentity())
	mode := os.Getenv("MULTICA_PRODUCER_MODE")
	if mode == "physical-quota" {
		f, err := os.OpenFile(filepath.Join(stage, "quota-fill"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		block := make([]byte, 1<<20)
		var bytes int64
		for i := 0; i < 256; i++ {
			n, e := f.Write(block)
			bytes += int64(n)
			if e != nil {
				err = e
				break
			}
		}
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if !nativeProducerQuotaError(err) {
			t.Fatal("expected native capacity exhaustion", err)
		}
		t.Logf("PHYSICAL_QUOTA_DENIED written=%d error=%v", bytes, err)
		os.Exit(27)
	}
	producer := exec.Command(os.Getenv("MULTICA_PRODUCER_BINARY"), "--exact", "remote::broker_lab::produce", "--ignored", "--nocapture")
	producer.Env = os.Environ()
	producer.Dir = stage
	producer.Stdout, producer.Stderr = os.Stdout, os.Stderr
	if err := producer.Run(); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "symlink":
		if err := nativeDirectoryLink(filepath.Join(stage, "outside-link"), os.Getenv("MULTICA_PRODUCER_DEST")); err != nil {
			t.Fatal(err)
		}
	case "hardlink":
		if err := os.Link(filepath.Join(stage, "evidence.json"), filepath.Join(stage, "evidence-alias")); err != nil {
			t.Fatal(err)
		}
	case "partial":
		if err := os.WriteFile(filepath.Join(stage, "evidence.json"), []byte("{"), 0600); err != nil {
			t.Fatal(err)
		}
	case "logical-quota":
		f, err := os.Create(filepath.Join(stage, "oversized"))
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Truncate(9 << 20); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	case "held":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestBrokerProducerForker$")
		cmd.Env = os.Environ()
		cmd.Dir = stage
		nativeRuntimeDetach(cmd)
		if err = cmd.Run(); err != nil {
			t.Fatal(err)
		}
		waitRuntimeFile(t, filepath.Join(stage, "descendant-ready"))
	default:
		t.Fatal("unknown producer probe", mode)
	}
}
func TestBrokerProducerForker(t *testing.T) {
	stage := os.Getenv("MULTICA_PRODUCER_STAGE")
	if stage == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokerProducerDescendant$")
	cmd.Env = os.Environ()
	cmd.Dir = stage
	nativeRuntimeDetach(cmd)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Release(); err != nil {
		t.Fatal(err)
	}
}
func TestBrokerProducerDescendant(t *testing.T) {
	stage := os.Getenv("MULTICA_PRODUCER_STAGE")
	if stage == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("descendant is privileged")
	}
	labWrite(t, filepath.Join(stage, "descendant-ready"), strconv.Itoa(os.Getpid()))
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(stage, "release")); err == nil {
			return
		}
		if err := os.WriteFile(filepath.Join(stage, "heartbeat"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("producer descendant was not released")
}
