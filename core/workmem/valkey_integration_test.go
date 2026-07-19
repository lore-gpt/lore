//go:build integration

package workmem

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	valkey "github.com/valkey-io/valkey-go"
)

// startValkey starts a Valkey container and returns its redis:// URL (with cleanup registered).
func startValkey(ctx context.Context, t *testing.T) (string, testcontainers.Container) {
	t.Helper()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8.1-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start valkey: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate valkey: %v", err)
		}
	})
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("valkey host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("valkey port: %v", err)
	}
	url := "redis://" + host + ":" + port.Port()

	// ForListeningPort only proves the port accepts a TCP connection — a beat before Valkey answers commands.
	// valkey-go's client init (the RESP3 handshake) can still lose the tight 2s op timeout right after boot,
	// under load, which intermittently degraded an Open/first-op the tests expect to be Healthy (the CI flake in
	// TestValkeyStoreOpRedialsAndRecovers / TestValkeyStoreDegradesWhenUnreachable). Actively PING until the
	// server answers a command, so the container is proven command-ready before any test dials it.
	readyOpt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatalf("parse url for readiness probe: %v", err)
	}
	readyOpt.Dialer.Timeout = 5 * time.Second
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, derr := valkey.NewClient(readyOpt)
		if derr == nil {
			perr := c.Do(ctx, c.B().Ping().Build()).Error()
			c.Close()
			if perr == nil {
				break
			}
			derr = perr
		}
		if time.Now().After(deadline) {
			t.Fatalf("valkey never became command-ready within 30s: %v", derr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return url, ctr
}

// TestValkeyStoreRoundTrip proves the Valkey-backed store against a real server: Set/Get/GetAll round
// trip a hot fact with its provenance, a run's whole working section reads back, and other runs are
// isolated.
func TestValkeyStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	url, _ := startValkey(ctx, t)
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if s.Mode() != Healthy {
		t.Fatalf("Mode() = %v, want Healthy against a reachable server", s.Mode())
	}

	k := Key{ProjectID: "p", RunID: "r1", Entity: "auth", Predicate: "state"}
	if err := s.Set(ctx, k, Value{Value: []byte(`"up"`), Seq: 7, Agent: "planner"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(ctx, k)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(got.Value) != `"up"` || got.Seq != 7 || got.Agent != "planner" {
		t.Errorf("Get = %+v, want {\"up\",7,planner}", got)
	}

	// A missing subject reports not-found, not an error.
	if _, ok, err := s.Get(ctx, Key{ProjectID: "p", RunID: "r1", Entity: "nope", Predicate: "x"}); err != nil || ok {
		t.Errorf("Get(missing) = ok=%v err=%v, want (false, nil)", ok, err)
	}

	// GetAll returns the whole run's working section; a different run is isolated.
	_ = s.Set(ctx, Key{ProjectID: "p", RunID: "r1", Entity: "db", Predicate: "ver"}, Value{Value: []byte(`"2"`)})
	_ = s.Set(ctx, Key{ProjectID: "p", RunID: "r2", Entity: "auth", Predicate: "state"}, Value{Value: []byte(`"other"`)})
	all, err := s.GetAll(ctx, "p", "r1")
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("GetAll(r1) = %d entries, want 2", len(all))
	}
	var authState string
	for _, e := range all {
		if e.Entity == "auth" && e.Predicate == "state" {
			authState = string(e.Value.Value)
		}
	}
	if authState != `"up"` {
		t.Errorf("GetAll(r1) auth/state = %q, want \"up\"", authState)
	}
	if r2, _ := s.GetAll(ctx, "p", "r2"); len(r2) != 1 {
		t.Errorf("GetAll(r2) = %d entries, want 1 (isolated)", len(r2))
	}
}

// TestValkeyStoreRefreshesIdleTTL proves every Set stamps the run hash with the idle TTL, so an
// abandoned run's hot keys expire instead of leaking. Uses a short TTL and reads the remaining TTL back.
func TestValkeyStoreRefreshesIdleTTL(t *testing.T) {
	ctx := context.Background()
	url, _ := startValkey(ctx, t)
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	vs := s.(*valkeyStore)
	vs.ttl = 100 * time.Second // short, deterministic to assert

	k := Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}
	if err := vs.Set(ctx, k, Value{Value: []byte(`"x"`)}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ttl, err := vs.client.Do(ctx, vs.client.B().Ttl().Key(vs.runHashKey("p", "r")).Build()).AsInt64()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl <= 0 || ttl > 100 {
		t.Errorf("run hash TTL = %ds, want (0,100] — Set must refresh the idle TTL", ttl)
	}
}

// TestValkeyStoreDegradesWhenUnreachable proves the two non-fatal degrade paths: Open against a server
// that is not listening returns a Degraded store (not an error, so the boot continues), and losing a
// reachable server mid-run flips a live store to Degraded on the next op.
func TestValkeyStoreDegradesWhenUnreachable(t *testing.T) {
	ctx := context.Background()

	// (1) Configured but unreachable at Open: Degraded store, no fatal error.
	down, err := Open(ctx, "redis://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Open(unreachable) returned a fatal error, want a Degraded store: %v", err)
	}
	t.Cleanup(down.Close)
	if down.Mode() != Degraded {
		t.Errorf("Mode() = %v, want Degraded for an unreachable server", down.Mode())
	}
	if err := down.Set(ctx, Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}, Value{Value: []byte(`"x"`)}); err == nil {
		t.Error("Set against an unreachable server should return an error (for the caller to count)")
	}

	// (2) Reachable, then the server dies: the next op flips Mode to Degraded.
	url, ctr := startValkey(ctx, t)
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if s.Mode() != Healthy {
		t.Fatalf("Mode() = %v, want Healthy", s.Mode())
	}
	if err := testcontainers.TerminateContainer(ctr); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if err := s.Set(ctx, Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}, Value{Value: []byte(`"x"`)}); err == nil {
		t.Error("Set after the server died should error")
	}
	if s.Mode() != Degraded {
		t.Errorf("Mode() = %v after the server died, want Degraded", s.Mode())
	}
}

// TestValkeyStoreOpRedialsAndRecovers proves the self-heal promise (the other direction of degrade): a
// store with no client yet — the state after a failed boot dial — and Degraded health re-dials on the
// first op and flips back to Healthy, with no restart. It is built directly (no probe loop) so the op is
// unambiguously what re-dials and restores health, killing a mutant that never re-dials or never records
// success. The server is real (a container); only the store's own client starts absent.
func TestValkeyStoreOpRedialsAndRecovers(t *testing.T) {
	ctx := context.Background()
	url, _ := startValkey(ctx, t)
	opt, err := valkey.ParseURL(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	opt.Dialer.Timeout = opTimeout

	// A store whose client was never created (as after an unreachable boot) and never probed: Degraded.
	vs := &valkeyStore{opt: opt, ttl: DefaultIdleTTL}
	t.Cleanup(func() {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		if vs.client != nil {
			vs.client.Close()
		}
	})
	if vs.Mode() != Degraded {
		t.Fatalf("Mode() = %v before any successful op, want Degraded", vs.Mode())
	}

	// An op must lazily create the client (ensureClient re-dial) and flip Healthy. The store self-heals ON USE —
	// ensureClient re-dials on each op until one connects — so poll briefly rather than demanding the single first
	// call beat a just-booted container: the contract is "recovers on use", not "the first call never races a cold
	// dependency". A store that NEVER re-dials fails every attempt in the window, so the mutant is still killed.
	k := Key{ProjectID: "p", RunID: "r", Entity: "e", Predicate: "pr"}
	deadline := time.Now().Add(10 * time.Second)
	var setErr error
	for {
		if setErr = vs.Set(ctx, k, Value{Value: []byte(`"x"`)}); setErr == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if setErr != nil {
		t.Fatalf("Set (re-dial on use): %v", setErr)
	}
	if vs.Mode() != Healthy {
		t.Errorf("Mode() = %v after a successful op, want Healthy (the op re-dials and records success)", vs.Mode())
	}
	if got, ok, _ := vs.Get(ctx, k); !ok || string(got.Value) != `"x"` {
		t.Errorf("Get after recovery = %q (ok=%v), want x", got.Value, ok)
	}
}
