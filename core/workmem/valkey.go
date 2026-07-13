package workmem

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	valkey "github.com/valkey-io/valkey-go"
)

const (
	// DefaultIdleTTL is how long a run's working set survives without a write before the cache evicts it.
	// Every Set refreshes it, so an active run never expires; an abandoned run's hot keys do not leak.
	DefaultIdleTTL = 6 * time.Hour
	// healthProbeEvery is how often the background probe re-checks reachability, so Mode() stays fresh
	// (and recovers after the cache comes back) even when no Set/Get traffic is flowing.
	healthProbeEvery = 5 * time.Second
	// opTimeout bounds a single cache round-trip (dial, probe, or command), so a stalled cache degrades
	// quickly rather than blocking a caller.
	opTimeout = 2 * time.Second
)

// Open builds a Store from a Valkey URL. An empty URL returns the no-op (Disabled) store, so a
// deployment without a cache runs unchanged. A malformed URL is a fatal config error; an UNREACHABLE
// server is NOT — Open returns a Degraded store that keeps trying to connect (the caller logs it and
// continues), because the working-memory stripe is optional and Postgres remains the durable authority.
func Open(ctx context.Context, url string) (Store, error) {
	if strings.TrimSpace(url) == "" {
		return noopStore{}, nil
	}
	opt, err := valkey.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("workmem: parse valkey url: %w", err)
	}
	opt.Dialer.Timeout = opTimeout // bound the dial so an unreachable server degrades fast, not blocks
	s := &valkeyStore{opt: opt, ttl: DefaultIdleTTL, stop: make(chan struct{}), done: make(chan struct{})}
	s.probe(ctx)              // initial connect+ping: sets Healthy, or Degraded without failing the boot
	s.initialized.Store(true) // arm transition logging: the boot state is announced by the caller, not here
	go s.probeLoop()
	return s, nil
}

// valkeyStore is the Valkey-backed Store. A run's hot facts live in one hash keyed by (project, run);
// each subject is a field. Every Set refreshes the hash's idle TTL, so the whole run's working set
// expires together once writes stop. The client is created lazily and rebuilt if the initial dial failed,
// so a cache that is down at boot (or comes back later) is handled without failing anything.
type valkeyStore struct {
	opt         valkey.ClientOption
	ttl         time.Duration
	mu          sync.Mutex    // guards client creation
	client      valkey.Client // nil until the first successful connect
	healthy     atomic.Bool
	initialized atomic.Bool // true once boot is done, so setHealthy logs only real (post-boot) transitions
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once // Close is idempotent: a store shared across roles must not double-close the stop channel
}

// setHealthy records reachability and logs a transition once — a WARN when the stripe goes unreachable, an
// INFO when it recovers — so operators see a state change without a per-op log flood (Degraded skips stay
// silent). The boot probe runs before initialized is armed, so the initial state is announced by the
// caller, not double-logged here.
func (s *valkeyStore) setHealthy(healthy bool) {
	if s.healthy.Swap(healthy) == healthy || !s.initialized.Load() {
		return
	}
	if healthy {
		slog.Info("working memory reachable")
	} else {
		slog.Warn("working memory unreachable; hot facts fall through to durable extraction")
	}
}

func (s *valkeyStore) runHashKey(projectID, runID string) string {
	return "wm:" + projectID + ":" + runID
}

// ensureClient returns a live client, creating it on first use (or after the initial dial failed). A live
// valkey client reconnects internally on transient blips, so this only re-dials when we never got one.
func (s *valkeyStore) ensureClient() valkey.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client
	}
	c, err := valkey.NewClient(s.opt)
	if err != nil {
		return nil
	}
	s.client = c
	return c
}

// probe checks reachability and records it; it never returns an error (health is a state, not a failure
// the caller acts on).
func (s *valkeyStore) probe(ctx context.Context) {
	c := s.ensureClient()
	if c == nil {
		s.setHealthy(false)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	s.setHealthy(c.Do(pctx, c.B().Ping().Build()).Error() == nil)
}

func (s *valkeyStore) probeLoop() {
	defer close(s.done)
	t := time.NewTicker(healthProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.probe(context.Background())
		}
	}
}

func (s *valkeyStore) Mode() Mode {
	if s.healthy.Load() {
		return Healthy
	}
	return Degraded
}

func (s *valkeyStore) Set(ctx context.Context, k Key, v Value) error {
	c := s.ensureClient()
	if c == nil {
		s.setHealthy(false)
		return fmt.Errorf("workmem: set: cache unreachable")
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("workmem: marshal value: %w", err)
	}
	key := s.runHashKey(k.ProjectID, k.RunID)
	cmds := []valkey.Completed{
		c.B().Hset().Key(key).FieldValue().FieldValue(field(k.Entity, k.Predicate), string(blob)).Build(),
		c.B().Expire().Key(key).Seconds(int64(s.ttl.Seconds())).Build(),
	}
	for _, res := range c.DoMulti(ctx, cmds...) {
		if err := res.Error(); err != nil {
			s.setHealthy(false)
			return fmt.Errorf("workmem: set: %w", err)
		}
	}
	s.setHealthy(true)
	return nil
}

func (s *valkeyStore) Get(ctx context.Context, k Key) (Value, bool, error) {
	c := s.ensureClient()
	if c == nil {
		s.setHealthy(false)
		return Value{}, false, fmt.Errorf("workmem: get: cache unreachable")
	}
	res := c.Do(ctx, c.B().Hget().Key(s.runHashKey(k.ProjectID, k.RunID)).Field(field(k.Entity, k.Predicate)).Build())
	blob, err := res.AsBytes()
	if valkey.IsValkeyNil(err) {
		s.setHealthy(true) // a reachable round-trip that simply found no field
		return Value{}, false, nil
	}
	if err != nil {
		s.setHealthy(false)
		return Value{}, false, fmt.Errorf("workmem: get: %w", err)
	}
	s.setHealthy(true)
	var v Value
	if err := json.Unmarshal(blob, &v); err != nil {
		return Value{}, false, fmt.Errorf("workmem: unmarshal value: %w", err)
	}
	return v, true, nil
}

func (s *valkeyStore) GetAll(ctx context.Context, projectID, runID string) ([]Entry, error) {
	c := s.ensureClient()
	if c == nil {
		s.setHealthy(false)
		return nil, fmt.Errorf("workmem: getall: cache unreachable")
	}
	res := c.Do(ctx, c.B().Hgetall().Key(s.runHashKey(projectID, runID)).Build())
	raw, err := res.AsStrMap()
	if err != nil {
		s.setHealthy(false)
		return nil, fmt.Errorf("workmem: getall: %w", err)
	}
	s.setHealthy(true)
	out := make([]Entry, 0, len(raw))
	for f, blob := range raw {
		entity, predicate, ok := parseField(f)
		if !ok {
			continue // skip a field not written by this store
		}
		var v Value
		if err := json.Unmarshal([]byte(blob), &v); err != nil {
			return nil, fmt.Errorf("workmem: unmarshal value: %w", err)
		}
		out = append(out, Entry{Entity: entity, Predicate: predicate, Value: v})
	}
	return out, nil
}

func (s *valkeyStore) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.client != nil {
			s.client.Close()
		}
	})
}
