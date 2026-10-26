//go:build cachebrokerlab && (darwin || windows)

package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/cachebrokerlab"
	"github.com/multica-ai/multica/server/pkg/agent"
)

var runtimeIdentityIndex atomic.Int32

func allocateRuntimeIdentity(t *testing.T, cfg nativeLab) runtimeIdentity {
	t.Helper()
	i := int(runtimeIdentityIndex.Add(1)) - 1
	if i >= len(cfg.RuntimeIdentities) {
		t.Fatal("disposable identity capacity exhausted")
	}
	return cfg.RuntimeIdentities[i]
}

type runtimeClientConfig struct {
	Control, Address, RunID, Root, Artifact string
	Resume, Recover                         bool
}

// The supervisor is this separate privileged harness process. Driver and cache
// service restarts cannot close its native containment handles or replay launch.
type runtimeGateway struct {
	supervisor *cachebrokerlab.Supervisor
	binding    cachebrokerlab.RunBinding
	config     runtimeClientConfig
	configPath string
	journal    string
	domain     *nativeRuntimeDomain
	done       chan error
}

func newRuntimeGateway(t *testing.T, s *serviceLab, root, artifact string, id runtimeIdentity, resume bool) *runtimeGateway {
	t.Helper()
	journal := filepath.Join(s.control, "runtime-"+fmt.Sprint(s.home.Current))
	if err := os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	nativePrivate(t, journal)
	supervisor, err := cachebrokerlab.OpenSupervisor(journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	tlsConfig, err := cachebrokerlab.ServiceTLS(s.control, true)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener = tls.NewListener(listener, tlsConfig)
	t.Cleanup(func() { listener.Close() })
	generation := s.home.Generations[s.home.Current]
	g := &runtimeGateway{journal: journal, supervisor: supervisor, binding: cachebrokerlab.RunBinding{RunID: generation.RunID, LeaseID: generation.LeaseID, Generation: generation.ID, Identity: nativeRuntimeIdentity(id)}, config: runtimeClientConfig{Control: s.control, Address: listener.Addr().String(), RunID: generation.RunID, Root: root, Artifact: artifact, Resume: resume}, done: make(chan error, 1)}
	g.domain = newNativeRuntimeDomain(t, s.cfg, id, root)
	g.configPath = filepath.Join(s.control, fmt.Sprintf("driver-%d.json", generation.ID))
	data, _ := json.Marshal(g.config)
	if err = os.WriteFile(g.configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(g.done)
		for i := 0; i < 2; i++ {
			g.done <- g.serve(listener, s, root, artifact)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		for range g.done {
		} // All run-owned transport work finishes here.
	})
	return g
}

func (g *runtimeGateway) serve(listener net.Listener, s *serviceLab, root, artifact string) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) == "proof:"+g.binding.RunID {
		done, proofErr := g.supervisor.Quiescent(g.binding)
		response := runtimeProof{Quiescent: done}
		if proofErr != nil {
			response.Error = proofErr.Error()
		}
		return json.NewEncoder(conn).Encode(response)
	}
	if strings.TrimSpace(line) != g.binding.RunID {
		return fmt.Errorf("unregistered runtime connection")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokerRuntimeFixture$")
	cmd.Dir = filepath.Join(root, "workspace")
	cmd.Env = append(nativeBaseEnv(), "MULTICA_RUNTIME_FIXTURE=1", "MULTICA_RUNTIME_ROOT="+root, "MULTICA_RUNTIME_ARTIFACT="+artifact, "MULTICA_RUNTIME_CONTROL="+s.control, "CODEX_HOME="+filepath.Join(root, "codex-home"), "HOME="+filepath.Join(root, "workspace"), "USERPROFILE="+filepath.Join(root, "workspace"), "TEMP="+filepath.Join(root, "workspace"), "TMP="+filepath.Join(root, "workspace"), "TMPDIR="+filepath.Join(root, "workspace"))
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err = g.supervisor.Launch(g.binding, cmd, g.domain); err != nil {
		return err
	}
	copied := make(chan struct{})
	go func() { io.Copy(in, reader); in.Close(); close(copied) }()
	_, copyErr := io.Copy(conn, out)
	waitErr := cmd.Wait()
	conn.Close()
	<-copied
	if waitErr != nil {
		return waitErr
	}
	return copyErr
}

type runtimeProof struct {
	Quiescent bool
	Error     string
}

func (g *runtimeGateway) driver(t *testing.T, recover bool) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := g.configPath
	if recover {
		cfg := g.config
		cfg.Recover = true
		path += ".recovery"
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokerRuntimeDriver$")
	cmd.Env = append(nativeBaseEnv(), "MULTICA_RUNTIME_CLIENT_CONFIG="+path)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}
func (g *runtimeGateway) finish(t *testing.T, s *serviceLab) bool {
	t.Helper()
	done, err := g.supervisor.Quiescent(g.binding)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		return false
	}
	operation := "end"
	if s.home.Generations[s.home.Current].Lifecycle == "unknown" {
		operation = "reconcile"
	}
	s.call(t, s.request(operation))
	time.Sleep(1100 * time.Millisecond)
	return true
}
func waitRuntimeFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("runtime fixture did not produce", filepath.Base(path))
}

func TestBrokerNativeRuntimeCycles(t *testing.T) {
	for _, route := range []string{"direct", "walk"} {
		for _, artifact := range []string{cachebrokerlab.Marketplaces, cachebrokerlab.Catalog} {
			t.Run(route+"/"+artifact, func(t *testing.T) {
				cfg := mustLab(t)
				identities := []runtimeIdentity{allocateRuntimeIdentity(t, cfg), allocateRuntimeIdentity(t, cfg)}
				s := newServiceLab(t, func(c *cachebrokerlab.ServiceConfig) {
					for _, id := range identities {
						c.RuntimeHomes = append(c.RuntimeHomes, cachebrokerlab.WritableHome{Group: id.GID, SID: id.SID})
					}
				})
				s.register(t)
				for cycle := 0; cycle < 2; cycle++ {
					intent := "launch"
					if cycle > 0 {
						intent = "restore"
					}
					root := s.begin(t, intent)
					nativeRuntimeHome(t, root, identities[cycle])
					prepared := s.prepare(t, artifact)
					var expected int64
					for _, e := range prepared.Receipt.Entries {
						expected += e.Bytes
					}
					if expected <= 0 {
						t.Fatal("empty producer evidence")
					}
					g := newRuntimeGateway(t, s, root, artifact, identities[cycle], cycle > 0)
					driver := g.driver(t, false)
					waitRuntimeFile(t, filepath.Join(root, "workspace", "descendant-ready"))
					waitRuntimeFile(t, filepath.Join(root, "workspace", "driver-result"))
					if err := driver.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					if err := driver.Wait(); err == nil {
						t.Fatal("driver crash not observed")
					}
					if err := <-g.done; err != nil {
						t.Fatal("launch transport", err)
					}
					recovered := g.driver(t, true)
					if err := recovered.Wait(); err != nil {
						t.Fatal("restarted driver", err)
					}
					waitRuntimeFile(t, filepath.Join(root, "workspace", "driver-recovery"))
					if err := <-g.done; err != nil {
						t.Fatal("recovery transport", err)
					}
					if g.finish(t, s) {
						t.Fatal("adapter Result authorized GC with live detached descendant")
					}
					restoreQuery := nativeRuntimeFailQuery(g.domain)
					done, queryErr := g.supervisor.Quiescent(g.binding)
					restoreQuery()
					if done || queryErr == nil {
						t.Fatal("failed native query authorized terminal")
					}
					r := s.request("reclaim")
					r.Artifact = artifact
					s.deny(t, r)
					before := labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(artifact)))
					if n, bytes := s.gcExpect(t, root, route, artifact, true); n != 0 || bytes != 0 {
						t.Fatal("GC changed active runtime bytes")
					}
					s.restart(t)
					if g.finish(t, s) {
						t.Fatal("service restart authorized GC with live detached descendant")
					}
					r = s.request("reclaim")
					r.Artifact = artifact
					s.deny(t, r)
					if n, bytes := s.gcExpect(t, root, route, artifact, true); n != 0 || bytes != 0 {
						t.Fatal("GC changed restarted active runtime bytes")
					}
					if before != labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(artifact))) {
						t.Fatal("active cache changed across restart")
					}
					if err := os.WriteFile(filepath.Join(root, "workspace", "release"), []byte("release test child"), 0644); err != nil {
						t.Fatal(err)
					}
					deadline := time.Now().Add(20 * time.Second)
					finished := false
					for time.Now().Before(deadline) {
						if g.finish(t, s) {
							finished = true
							break
						}
						time.Sleep(20 * time.Millisecond)
					}
					if !finished {
						t.Fatal("native containment never became quiescent")
					}
					if n, bytes := s.gc(t, root, route, artifact); n != 1 || bytes != expected {
						t.Fatalf("runtime GC %d/%d want1/%d", n, bytes, expected)
					}
					if data, err := os.ReadFile(filepath.Join(root, "codex-home", "task-created.json")); err != nil || string(data) != "task-owned" {
						t.Fatal("writable home output lost", err)
					}
					t.Logf("RUNTIME_CYCLE route=%s artifact=%s cycle=%d real_multica_adapter=true offline_protocol_fixture=true detached_descendant_retained=true driver_crash_restart_active=true service_restart_active=true logical_bytes=%d", route, artifact, cycle+1, expected)
				}
			})
		}
	}
}

func TestBrokerRuntimeDriver(t *testing.T) {
	path := os.Getenv("MULTICA_RUNTIME_CLIENT_CONFIG")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg runtimeClientConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Recover {
		config, err := cachebrokerlab.ServiceTLS(cfg.Control, false)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := tls.Dial("tcp", cfg.Address, config)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = fmt.Fprintln(conn, "proof:"+cfg.RunID); err != nil {
			t.Fatal(err)
		}
		var proof runtimeProof
		if err = json.NewDecoder(conn).Decode(&proof); err != nil {
			t.Fatal(err)
		}
		if proof.Quiescent || proof.Error != "" {
			t.Fatalf("restart did not retain live domain: %+v", proof)
		}
		if err = os.WriteFile(filepath.Join(cfg.Root, "workspace", "driver-recovery"), []byte("live descendant retains lease"), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := agent.New("codex", agent.Config{ExecutablePath: exe, LaunchPrefix: []string{"-test.run=^TestBrokerRuntimeTransport$", "--"}, CodexVersion: "0.163.0", Env: map[string]string{"MULTICA_RUNTIME_TRANSPORT": path, "CODEX_HOME": filepath.Join(cfg.Root, "codex-home")}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	opts := agent.ExecOptions{Cwd: filepath.Join(cfg.Root, "workspace"), HandshakeTimeout: 15 * time.Second, ThreadHandshakeTimeout: 15 * time.Second, Timeout: 30 * time.Second}
	if cfg.Resume {
		opts.ResumeSessionID = "runtime-fixture-thread"
	}
	session, err := backend.Execute(context.Background(), "offline runtime containment proof", opts)
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "completed" {
		t.Fatalf("real Codex adapter failed: %+v", result)
	}
	if err = os.WriteFile(filepath.Join(cfg.Root, "workspace", "driver-result"), []byte(result.Status), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Second) // The parent must kill this exact driver after Result.
	t.Fatal("driver crash was not delivered")
}
func TestBrokerRuntimeTransport(t *testing.T) {
	path := os.Getenv("MULTICA_RUNTIME_TRANSPORT")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg runtimeClientConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	config, err := cachebrokerlab.ServiceTLS(cfg.Control, false)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", cfg.Address, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintln(conn, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	go func() { io.Copy(conn, os.Stdin); conn.Close() }()
	_, err = io.Copy(os.Stdout, conn)
	if err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestBrokerRuntimeFixture(t *testing.T) {
	if os.Getenv("MULTICA_RUNTIME_FIXTURE") == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("runtime fixture privileged")
	}
	root := os.Getenv("MULTICA_RUNTIME_ROOT")
	artifact := os.Getenv("MULTICA_RUNTIME_ARTIFACT")
	home := filepath.Join(root, "codex-home")
	if err := os.WriteFile(filepath.Join(home, "task-created.json"), []byte("task-owned"), 0600); err != nil {
		t.Fatal("Codex home is not writable", err)
	}
	for _, p := range []string{home, filepath.Join(home, ".tmp"), filepath.Join(home, "cache")} {
		if err := os.Mkdir(filepath.Join(p, "task-created-directory"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, parent := range []string{".tmp", "cache"} {
		if err := os.Rename(filepath.Join(home, parent), filepath.Join(home, "replaced-"+parent)); err == nil {
			t.Fatal("protected cache parent replaced", parent)
		}
	}
	if _, err := os.ReadFile(filepath.Join(os.Getenv("MULTICA_RUNTIME_CONTROL"), "client.key")); err == nil {
		t.Fatal("runtime read daemon credential")
	}
	if artifact == cachebrokerlab.Marketplaces {
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(artifact), "cache-lab/plugins/sample/marker.txt")); err != nil || string(b) != "immutable local fixture" {
			t.Fatal("marketplace not restored before runtime discovery", err)
		}
	} else {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(artifact)))
		if err != nil || len(entries) != 1 {
			t.Fatal("catalog not fetched before runtime", err)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			t.Fatal(err)
		}
		if len(req.ID) == 0 {
			continue
		}
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			result = map[string]any{"userAgent": "offline-runtime-fixture"}
		case "thread/start", "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": "runtime-fixture-thread"}}
		case "turn/start":
			spawnRuntimeDescendant(t, root)
			result = map[string]any{"turn": map[string]any{"id": "runtime-fixture-turn", "status": "inProgress"}}
		}
		b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
		fmt.Println(string(b))
		if req.Method == "turn/start" {
			fmt.Println(`{"method":"turn/started","params":{"threadId":"runtime-fixture-thread","turn":{"id":"runtime-fixture-turn","status":"inProgress"}}}`)
			fmt.Println(`{"method":"turn/completed","params":{"threadId":"runtime-fixture-thread","turn":{"id":"runtime-fixture-turn","status":"completed"}}}`)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
func TestBrokerRuntimeDescendant(t *testing.T) {
	root := os.Getenv("MULTICA_RUNTIME_DESCENDANT")
	if root == "" {
		return
	}
	if nativePrivileged() {
		t.Fatal("descendant privileged")
	}
	nativeRuntimeEscapeProbe(t)
	if err := os.WriteFile(filepath.Join(root, "workspace", "descendant-ready"), []byte("detached child alive"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "workspace", "release")); err == nil {
			os.Exit(0)
		}
		if _, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(os.Getenv("MULTICA_RUNTIME_ARTIFACT")))); err != nil {
			t.Fatal("active child lost cache", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("supervisor did not release test descendant")
}

func spawnRuntimeDescendant(t *testing.T, root string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "TestBrokerRuntimeForker"
	if os.Getenv("MULTICA_RUNTIME_FORKER") != "" {
		name = "TestBrokerRuntimeDescendant"
	}
	cmd := exec.Command(exe, "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), "MULTICA_RUNTIME_DESCENDANT="+root, "MULTICA_RUNTIME_FORKER=1")
	cmd.Dir = filepath.Join(root, "workspace")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	nativeRuntimeDetach(cmd)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if name == "TestBrokerRuntimeForker" {
		if err = cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err = cmd.Process.Release(); err != nil {
			t.Fatal(err)
		}
	}
}
func TestBrokerRuntimeForker(t *testing.T) {
	root := os.Getenv("MULTICA_RUNTIME_DESCENDANT")
	if root == "" {
		return
	}
	spawnRuntimeDescendant(t, root)
	os.Exit(0)
}

// Reopening durable state has no authority to reconstruct a lost native domain.
// This reopens the supervisor journal, not the whole test harness process.
func TestBrokerNativeRuntimeSupervisorRecovery(t *testing.T) {
	cfg := mustLab(t)
	id := allocateRuntimeIdentity(t, cfg)
	s := newServiceLab(t, func(c *cachebrokerlab.ServiceConfig) {
		c.RuntimeHomes = []cachebrokerlab.WritableHome{{Group: id.GID, SID: id.SID}}
	})
	s.register(t)
	root := s.begin(t, "launch")
	nativeRuntimeHome(t, root, id)
	s.prepare(t, cachebrokerlab.Catalog)
	g := newRuntimeGateway(t, s, root, cachebrokerlab.Catalog, id, false)
	driver := g.driver(t, false)
	waitRuntimeFile(t, filepath.Join(root, "workspace", "descendant-ready"))
	waitRuntimeFile(t, filepath.Join(root, "workspace", "driver-result"))
	if err := driver.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := driver.Wait(); err == nil {
		t.Fatal("driver did not crash")
	}
	if err := <-g.done; err != nil {
		t.Fatal(err)
	}
	recovery := g.driver(t, true)
	if err := recovery.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-g.done; err != nil {
		t.Fatal(err)
	}
	before := labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog)))
	if err := g.supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := cachebrokerlab.OpenSupervisor(g.journal)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if done, err := recovered.Quiescent(g.binding); done || err == nil {
		t.Fatal("recovery invented containment proof")
	}
	if err := recovered.Launch(g.binding, &exec.Cmd{}, g.domain); err == nil {
		t.Fatal("recovery replayed launch")
	}
	s.restart(t)
	if n, bytes := s.gcExpect(t, root, "direct", cachebrokerlab.Catalog, true); n != 0 || bytes != 0 {
		t.Fatal("uncertain supervisor granted GC")
	}
	if err := os.WriteFile(filepath.Join(root, "workspace", "release"), []byte("release"), 0644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		n, err := g.domain.Active()
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if done, err := recovered.Quiescent(g.binding); done || err == nil {
		t.Fatal("lost domain inferred completion from elapsed time")
	}
	if before != labTreeSnapshot(t, filepath.Join(root, filepath.FromSlash(cachebrokerlab.Catalog))) {
		t.Fatal("recovery changed retained cache")
	}
	t.Log("RUNTIME_UNCERTAIN supervisor_journal_reopened=true launch_replay_denied=true lost_domain_retained=true byte_credit=0")
}
