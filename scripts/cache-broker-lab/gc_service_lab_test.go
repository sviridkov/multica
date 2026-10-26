//go:build cachebrokerlab && (darwin || windows)

package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

type serviceLab struct {
	cfg                   nativeLab
	control, state, epoch string
	start, stop           func()
	client                *cachebrokerlab.Client
	sequence              int
	home                  *cachebrokerlab.Home
	stages                []serviceAcquisitionSlot
	storage               *cachebrokerlab.StorageLayout
}

type serviceAcquisitionSlot struct {
	Generation              uint64
	Artifact, Stage, Volume string
	Identity                runtimeIdentity
}

func newServiceLab(t *testing.T, options ...func(*cachebrokerlab.ServiceConfig)) *serviceLab {
	t.Helper()
	cfg := mustLab(t)
	var recipe cachebrokerlab.Policy
	root, b := labBroker(t, cfg, func(p *cachebrokerlab.Policy) { recipe = *p })
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	dataVolume := nativeProducerVolume(t, cfg)
	serviceRoot, err := os.MkdirTemp(dataVolume, "service-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(serviceRoot, 0755); err != nil {
		t.Fatal(err)
	}
	s := &serviceLab{cfg: cfg, control: filepath.Join(serviceRoot, "control"), state: filepath.Join(serviceRoot, "state")}
	for _, path := range []string{s.control, s.state} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	nativePrivate(t, s.control)
	if err := cachebrokerlab.CreateLabIdentity(s.control); err != nil {
		t.Fatal(err)
	}
	serviceConfig := cachebrokerlab.ServiceConfig{Root: s.state, RecipeID: "codex-pinned-v1", Recipe: recipe, ArtifactTTLSeconds: 1}
	for _, option := range options {
		option(&serviceConfig)
	}
	volume := nativeProducerVolume(t, cfg)
	for generation := uint64(1); generation <= 2; generation++ {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			id := allocateRuntimeIdentity(t, cfg)
			s.stages = append(s.stages, serviceAcquisitionSlot{Generation: generation, Artifact: artifact, Stage: nativeProducerStage(t, volume, id), Volume: volume, Identity: id})
		}
	}
	layout := cachebrokerlab.StorageLayout{BudgetBytes: 256 << 20, Control: s.control,
		Volumes: []cachebrokerlab.StorageVolume{
			{Root: dataVolume, LimitBytes: 128 << 20, MinFreeBytes: 16 << 20},
			{Root: volume, LimitBytes: 128 << 20, MinFreeBytes: 16 << 20},
		}}
	for _, slot := range s.stages {
		layout.Slots = append(layout.Slots, cachebrokerlab.StorageSlot{Generation: slot.Generation, Artifact: slot.Artifact, Stage: slot.Stage, Volume: slot.Volume})
	}
	serviceConfig.Storage, err = cachebrokerlab.CaptureStorage(s.state, layout)
	if err != nil {
		t.Fatal(err)
	}
	s.storage = serviceConfig.Storage
	t.Logf("STORAGE_REGISTRY budget_bytes=%d volumes=%d slots=%d state_control_bounded=true", s.storage.BudgetBytes, len(s.storage.Volumes), len(s.storage.Slots))
	data, _ := json.Marshal(struct {
		Service      cachebrokerlab.ServiceConfig
		Control      string
		Census       string
		Acquisitions []serviceAcquisitionSlot
	}{serviceConfig, s.control, cfg.Census, s.stages})
	if err := os.WriteFile(filepath.Join(s.control, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s.start, s.stop = nativeService(t, cfg, s.control, "cache-broker-"+filepath.Base(root))
	s.start()
	s.connect(t, "")
	t.Cleanup(func() {
		if s.client != nil {
			s.client.Close()
		}
	})
	return s
}

func (s *serviceLab) connect(t *testing.T, old string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := cachebrokerlab.NewClient(s.control)
		if err == nil {
			r, e := c.Call(context.Background(), cachebrokerlab.Request{Operation: "hello"})
			if e == nil && r.Error == "" && r.Epoch != "" && r.Epoch != old {
				s.client = c
				s.epoch = r.Epoch
				return
			}
			last = e
			c.Close()
		} else {
			last = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(s.control, "service.log"))
	t.Fatalf("native service readiness: %v %s", last, log)
}
func (s *serviceLab) restart(t *testing.T) {
	t.Helper()
	old := s.epoch
	s.client.Close()
	s.stop()
	s.start()
	s.connect(t, old)
	if s.home != nil {
		s.call(t, cachebrokerlab.Request{Operation: "query", HomeID: s.home.ID})
	}
}
func (s *serviceLab) request(operation string) cachebrokerlab.Request {
	s.sequence++
	r := cachebrokerlab.Request{Operation: operation, Epoch: s.epoch, RequestID: fmt.Sprintf("request-%d", s.sequence)}
	if s.home != nil {
		r.HomeID = s.home.ID
		r.Revision = s.home.Revision
		r.Generation = s.home.Current
		if g := s.home.Generations[s.home.Current]; g != nil {
			r.LeaseID = g.LeaseID
			r.RunID = g.RunID
		}
	}
	return r
}
func (s *serviceLab) call(t *testing.T, r cachebrokerlab.Request) cachebrokerlab.Response {
	t.Helper()
	resp, err := s.client.Call(context.Background(), r)
	if err != nil || resp.Error != "" {
		t.Fatalf("IPC %s: %v %s", r.Operation, err, resp.Error)
	}
	if resp.Home != nil {
		s.home = resp.Home
	}
	return resp
}
func (s *serviceLab) register(t *testing.T) {
	r := s.request("register")
	r.RecipeID = "codex-pinned-v1"
	s.call(t, r)
}
func (s *serviceLab) begin(t *testing.T, intent string) string {
	r := s.request("begin")
	r.RunID = fmt.Sprintf("run-%d", s.sequence)
	r.Intent = intent
	return s.call(t, r).Path
}
func (s *serviceLab) prepare(t *testing.T, artifact string) cachebrokerlab.Response {
	r := s.request("prepare")
	r.Artifact = artifact
	resp := s.call(t, r)
	if resp.Receipt == nil || resp.Receipt.Generation != s.home.Current {
		t.Fatal("acquisition receipt does not bind the service generation")
	}
	s.verifyAcquisitionIdentity(t, s.home.Current, artifact)
	return resp
}

func (s *serviceLab) verifyAcquisitionIdentity(t *testing.T, generation uint64, artifact string) {
	t.Helper()
	for _, slot := range s.stages {
		if slot.Generation != generation || slot.Artifact != artifact {
			continue
		}
		identity := nativeRuntimeIdentity(slot.Identity)
		log, err := os.ReadFile(filepath.Join(s.control, "service.log"))
		if err != nil || !strings.Contains(string(log), "SERVICE_PRODUCER ordinary=true identity="+identity+"\n") {
			t.Fatal("service did not execute acquisition under its provisioned ordinary identity", identity, err)
		}
		t.Logf("STAGED_SERVICE_IDENTITY identity=%s generation=%d artifact=%s", identity, generation, artifact)
		return
	}
	t.Fatal("missing acquisition identity")
}
func (s *serviceLab) end(t *testing.T) {
	s.call(t, s.request("end"))
	time.Sleep(1100 * time.Millisecond)
}
func (s *serviceLab) deny(t *testing.T, r cachebrokerlab.Request) {
	t.Helper()
	resp, err := s.client.Call(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == "" || resp.Outcome.Removed || resp.Outcome.Bytes != 0 {
		t.Fatalf("unsafe command accepted: %+v", resp)
	}
}

func (s *serviceLab) gc(t *testing.T, root, route, artifact string) (int, int64) {
	t.Helper()
	return s.gcExpect(t, root, route, artifact, false)
}

func (s *serviceLab) gcExpect(t *testing.T, root, route, artifact string, denied bool) (int, int64) {
	t.Helper()
	d := newGCTestDaemon(t, http.NewServeMux())
	reclaim := func(ctx context.Context, rel string) (cachebrokerlab.Outcome, error) {
		if rel != artifact {
			return cachebrokerlab.Outcome{Reason: "not materialized"}, nil
		}
		r := s.request("reclaim")
		r.Artifact = rel
		if denied {
			resp, err := s.client.Call(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Error == "" || resp.Outcome.Removed || resp.Outcome.Bytes != 0 {
				t.Fatal("GC accepted uncertain lifecycle")
			}
			return cachebrokerlab.Outcome{Reason: resp.Error}, nil
		}
		resp := s.call(t, r)
		return resp.Outcome, nil
	}
	if route == "direct" {
		n, b, _ := d.cleanManagedTaskArtifactsWithBroker(context.Background(), root, removeManagedArtifact, reclaim)
		return n, b
	}
	matcher := newArtifactMatcher(nil, execenv.ManagedReclaimableArtifactSubpaths())
	n, b, _ := d.cleanTaskArtifactsMatchingWithBroker(context.Background(), root, matcher, removeManagedArtifact, reclaim)
	return n, b
}

func TestBrokerNativeServiceCycles(t *testing.T) {
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				s := newServiceLab(t)
				s.register(t)
				var first string
				for cycle := 1; cycle <= 2; cycle++ {
					intent := "launch"
					if cycle == 2 {
						intent = "restore"
					}
					root := s.begin(t, intent)
					if root == first {
						t.Fatal("generation namespace reused")
					}
					if cycle == 1 {
						first = root
					}
					prepared := s.prepare(t, artifact)
					if prepared.Receipt == nil || !prepared.Receipt.Source.ReadVerified {
						t.Fatal("no producer readback before discovery")
					}
					var want int64
					for _, e := range prepared.Receipt.Entries {
						want += e.Bytes
					}
					if want <= 0 {
						t.Fatal("empty fixture")
					}
					finish := s.useChild(t, root, artifact)
					r := s.request("reclaim")
					r.Artifact = artifact
					s.deny(t, r)
					for _, role := range []string{"runtime", "neighbour"} {
						s.attack(t, root, role)
					}
					finish() // Trusted driver observes task exit before its terminal assertion.
					s.end(t)
					stop := startConcurrentAttack(t, s.cfg, root, artifact)
					n, bytes := s.gc(t, root, route, artifact)
					stop()
					if n != 1 || bytes != want {
						t.Fatalf("GC %d/%d want1/%d", n, bytes, want)
					}
					if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(artifact))); !os.IsNotExist(err) {
						t.Fatal("artifact survived", err)
					}
					t.Logf("SERVICE_CYCLE route=%s artifact=%s cycle=%d generation=%d logical_bytes=%d native_service=true authenticated_ipc=true", route, artifact, cycle, s.home.Current, bytes)
					old := s.request("reclaim")
					old.Artifact = artifact
					s.deny(t, old)
				}
			})
		}
	}
}

func (s *serviceLab) attack(t *testing.T, root, role string) {
	t.Helper()
	fs := nativeAttackCommand(t, s.cfg, root, role)
	out, err := fs.CombinedOutput()
	t.Logf("%s filesystem: %s", role, out)
	if err != nil {
		t.Fatal(err)
	}
	cmd := nativeAttackCommand(t, s.cfg, root, role)
	cmd.Args = []string{cmd.Path, "-test.run=^TestBrokerServiceAttackChild$", "-test.v"}
	ca, err := os.ReadFile(filepath.Join(s.control, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "MULTICA_SERVICE_CONTROL="+s.control, "MULTICA_SERVICE_STATE="+s.state, "MULTICA_SERVICE_ENDPOINT="+s.client.Endpoint, "MULTICA_SERVICE_CA="+base64.StdEncoding.EncodeToString(ca))
	out, err = cmd.CombinedOutput()
	t.Logf("%s IPC: %s", role, out)
	if err != nil {
		t.Fatal(err)
	}
}
func TestBrokerServiceAttackChild(t *testing.T) {
	control := os.Getenv("MULTICA_SERVICE_CONTROL")
	if control == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("ordinary IPC attacker is privileged")
	}
	for _, name := range []string{"client.key", "server.key", "config.json"} {
		if _, err := os.ReadFile(filepath.Join(control, name)); err == nil {
			t.Fatal("stole private credential/config")
		}
	}
	for _, path := range []string{control, filepath.Join(control, "client.key"), filepath.Join(control, "fault.json"), os.Getenv("MULTICA_SERVICE_STATE"), filepath.Join(os.Getenv("MULTICA_SERVICE_STATE"), "registry.json")} {
		if err := os.WriteFile(path, []byte("forged"), 0600); err == nil {
			t.Fatal("changed protected service state")
		}
		if err := nativePermissionAttack(path); err == nil {
			t.Fatal("changed protected service ACL")
		}
	}
	ca, err := base64.StdEncoding.DecodeString(os.Getenv("MULTICA_SERVICE_CA"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("missing public CA")
	}
	c := &cachebrokerlab.Client{Endpoint: os.Getenv("MULTICA_SERVICE_ENDPOINT"), HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}, Timeout: 10 * time.Second}}
	defer c.Close()
	for _, op := range []string{"register", "begin", "prepare", "end", "reconcile", "reclaim"} {
		if _, err := c.Call(context.Background(), cachebrokerlab.Request{Operation: op}); err == nil {
			t.Fatalf("unauthenticated %s accepted", op)
		}
	}
	fmt.Printf("SERVICE_ATTACK identity=%s role=%s credentials_unreadable=true IPC_denied=6 filesystem_denied=true\n", nativeIdentity(), os.Getenv("MULTICA_BROKER_ATTACK_ROLE"))
}

func (s *serviceLab) useChild(t *testing.T, root, artifact string) func() {
	t.Helper()
	cmd := nativeAttackCommand(t, s.cfg, root, "runtime-discovery")
	cmd.Args = []string{cmd.Path, "-test.run=^TestBrokerServiceUseChild$"}
	cmd.Env = append(cmd.Env, "MULTICA_SERVICE_ARTIFACT="+artifact)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	finish := func() {
		if done {
			return
		}
		done = true
		in.Close()
		rest, e := io.ReadAll(out)
		err := cmd.Wait()
		if err != nil || e != nil {
			t.Errorf("task lifecycle: %v %v %s", err, e, rest)
		}
	}
	t.Cleanup(finish)
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "DISCOVERED\n" {
		t.Fatalf("task discovery not ready: %q %v", line, err)
	}
	return finish
}
func TestBrokerServiceUseChild(t *testing.T) {
	artifact := os.Getenv("MULTICA_SERVICE_ARTIFACT")
	if artifact == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("task reader privileged")
	}
	root := filepath.Join(os.Getenv("MULTICA_BROKER_ATTACK_ROOT"), filepath.FromSlash(artifact))
	if artifact == cachebrokerlab.Marketplaces {
		data, err := os.ReadFile(filepath.Join(root, "cache-lab", "plugins", "sample", "marker.txt"))
		if err != nil || string(data) != "immutable local fixture" {
			t.Fatal("restore missing before discovery", err)
		}
	} else {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 {
			t.Fatal("catalog not fetched", err)
		}
		data, err := os.ReadFile(filepath.Join(root, entries[0].Name()))
		if err != nil || !json.Valid(data) {
			t.Fatal("catalog not readable", err)
		}
	}
	fmt.Println("DISCOVERED")
	io.Copy(io.Discard, os.Stdin)
}

func TestBrokerNativeServiceRestartAndUnknown(t *testing.T) {
	s := newServiceLab(t)
	s.register(t)
	root := s.begin(t, "launch")
	s.prepare(t, cachebrokerlab.Catalog)
	s.end(t)
	before := labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog)))
	old := s.request("reclaim")
	old.Artifact = cachebrokerlab.Catalog
	s.restart(t)
	s.deny(t, old)
	r := s.request("reclaim")
	r.Artifact = cachebrokerlab.Catalog
	s.deny(t, r)
	if before != labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog))) {
		t.Fatal("restart mutated retained READY cache")
	}
	s.call(t, s.request("reconcile"))
	time.Sleep(1100 * time.Millisecond)
	if n, b := s.gc(t, root, "direct", cachebrokerlab.Catalog); n != 1 || b <= 0 {
		t.Fatal("reconciled cache unavailable")
	}
	newRoot := s.begin(t, "restore")
	unknown := filepath.Join(newRoot, filepath.FromSlash(cachebrokerlab.Catalog), "owner-notes")
	labWrite(t, unknown, "retain unknown content")
	r = s.request("prepare")
	r.Artifact = cachebrokerlab.Catalog
	resp, err := s.client.Call(context.Background(), r)
	if err != nil || resp.Error == "" {
		t.Fatal("adopted unknown artifact", err)
	}
	s.home = resp.Home
	data, err := os.ReadFile(unknown)
	if err != nil || string(data) != "retain unknown content" {
		t.Fatal("unknown data changed")
	}
	t.Log("SERVICE_RECOVERY READY retained; old epoch denied; trusted reconciliation required; unknown data quarantined")
}

func TestBrokerNativeServiceFaults(t *testing.T) {
	points := []string{"begin.intent", "begin.namespace", "prepare.intent", "production.intent", "production.running", "production.quiescent", "publication.before-commit", "publication.after-commit", "prepare.response", "reclaim.intent", "reclaim.consumed", "unlink.before", "unlink.after", "unlink.after-third", "reclaim.absent", "reclaim.response"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			s := newServiceLab(t)
			s.register(t)
			root := ""
			artifact := cachebrokerlab.Marketplaces
			op := "begin"
			if !strings.HasPrefix(point, "begin.") {
				root = s.begin(t, "launch")
				op = "prepare"
			}
			if strings.HasPrefix(point, "reclaim.") || strings.HasPrefix(point, "unlink.") {
				s.prepare(t, artifact)
				s.end(t)
				op = "reclaim"
			}
			request := s.request(op)
			request.Artifact = artifact
			if op == "begin" {
				request.Intent = "launch"
				request.RunID = "fault-run"
			}
			hook, after := point, 1
			if point == "unlink.after-third" {
				hook, after = "unlink.after", 3
			}
			data, _ := json.Marshal(map[string]any{"ID": point, "Point": hook, "Mode": "exit", "After": after})
			if err := os.WriteFile(filepath.Join(s.control, "fault.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.client.Call(context.Background(), request); err == nil {
				t.Fatal("service did not crash at armed boundary")
			}
			nativeWaitServiceTree(t, s.control)
			// Capture exactly what remains before recovery; restart must not mutate it.
			var before string
			target := filepath.Join(root, filepath.FromSlash(artifact))
			if root != "" {
				if _, err := os.Stat(target); err == nil {
					before = labTreeSnapshot(t, target)
				}
			}
			s.restart(t)
			result, err := s.client.Call(context.Background(), cachebrokerlab.Request{Operation: "result", RequestID: request.RequestID})
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(point, ".response") {
				if result.Error != "" {
					t.Fatal("lost a committed response", result.Error)
				}
				if op == "reclaim" && (!result.Outcome.Removed || result.Outcome.Bytes <= 0) {
					t.Fatal("lost committed reclaim outcome")
				}
				if op == "prepare" && (result.Receipt == nil || result.Receipt.State != "committed") {
					t.Fatal("lost committed preparation outcome")
				}
			} else if result.Error == "" || result.Outcome.Removed || result.Outcome.Bytes != 0 {
				t.Fatal("invented completion after interrupted operation")
			}
			if before != "" && before != labTreeSnapshot(t, target) {
				t.Fatal("restart deleted uncertain remnants")
			}
			old := request
			old.Epoch = s.epoch
			old.RequestID = "old-generation"
			old.Revision = s.home.Revision
			old.Operation = "reclaim"
			old.Generation = s.home.Current
			s.deny(t, old)
			if s.home.Current > 0 {
				s.call(t, s.request("reconcile"))
			}
			fresh := s.begin(t, "restore")
			if fresh == root {
				t.Fatal("reused crash generation")
			}
			s.prepare(t, cachebrokerlab.Catalog)
			s.end(t)
			if n, b := s.gc(t, fresh, "walk", cachebrokerlab.Catalog); n != 1 || b <= 0 {
				t.Fatal("fresh generation did not recover availability")
			}
			t.Logf("SERVICE_FAULT point=%s actual_process_exit=true restart=true remnants_preserved=true replacement_cycle=true", point)
		})
	}
}

func TestBrokerNativeServiceIOFailures(t *testing.T) {
	for _, point := range []string{"registry.before-write", "registry.before-sync", "registry.before-replace", "registry.after-replace", "receipt.before-write", "receipt.before-sync", "receipt.before-replace", "receipt.after-replace"} {
		t.Run(point, func(t *testing.T) {
			s := newServiceLab(t)
			s.register(t)
			s.begin(t, "launch")
			data, _ := json.Marshal(map[string]any{"ID": point, "Point": point, "Mode": "error", "After": 1})
			if err := os.WriteFile(filepath.Join(s.control, "fault.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			r := s.request("prepare")
			r.Artifact = cachebrokerlab.Catalog
			resp, err := s.client.Call(context.Background(), r)
			if err != nil || resp.Error == "" || resp.Outcome.Removed || resp.Outcome.Bytes != 0 {
				t.Fatal("IO failure reported success", err)
			}
			if resp.Home != nil {
				s.home = resp.Home
			}
			if strings.HasPrefix(point, "registry.") {
				s.deny(t, s.request("end"))
			}
			s.restart(t)
			r = s.request("reclaim")
			r.Artifact = cachebrokerlab.Catalog
			s.deny(t, r)
			s.call(t, s.request("reconcile"))
			root := s.begin(t, "restore")
			s.prepare(t, cachebrokerlab.Catalog)
			s.end(t)
			if n, b := s.gc(t, root, "direct", cachebrokerlab.Catalog); n != 1 || b <= 0 {
				t.Fatal("replacement failed after journal IO error")
			}
			t.Logf("SERVICE_IO_FAILURE point=%s no_false_success=true restart=true replacement_cycle=true", point)
		})
	}
}
