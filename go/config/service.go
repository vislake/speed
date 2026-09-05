package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// DefaultPollInterval is how often the anti-loss poller re-reads
// recently-updated configs rows when the host does not configure one. It is
// a documented package constant (the option is WithPollInterval): the
// design's TTL-fallback poller (docs/internal/11-cross-cutting.md's
// dynamic-config section) is the net under the event bus, so its
// cadence only bounds how long a lost event can leave a stale cache -- the
// poll interval itself is deliberately long enough to be a no-op in the
// common case where no event was ever lost.
const DefaultPollInterval = 30 * time.Second

// Service is the runtime face of the config module: the schema-driven,
// scope-resolved, cached, event-invalidated configuration store a host
// (and, later, other modules) reads and writes through. A Service is
// obtained from (*Module).Attach, which freezes the schema snapshot from
// the booted registry and wires the store, the bus subscription and the
// poller. Until Attach runs, no Service exists (see ErrServiceNotAttached
// for what a route served in that window reports).
//
// Everything the design's dynamic-config section asks of the runtime is
// here: Get resolves a key across scopes, from tenant override down to
// system row and schema default, Set validates against the schema and
// writes the configs row, every Set
// publishes config.item.changed (the hot-update signal other instances
// consume), the poller is the anti-loss net, and Sensitive values are
// encrypted at rest through the injected dbkit.Cipher. The Service is safe
// for concurrent use.
type Service struct {
	// schema is the frozen runtime snapshot; immutable after Attach.
	schema *schema

	// st is the configs table accessor.
	st *store

	// bus is the registry's EventBus, captured at Attach. Every Set
	// publishes on it; the Attach-time subscription consumes from it. In
	// the standalone deployment mode this is the in-memory bus, so a Set
	// delivers to this process's own subscriber synchronously inside the
	// Publish call -- watches fire before Set returns. Under the
	// distributed bus, delivery is asynchronous and the publishing
	// instance is just another subscriber.
	bus pkgcore.EventBus

	// cipher is the host's master-key cipher, nil when the host injected
	// none. Attach refuses a schema containing Sensitive items without one
	// (ErrCipherRequired), so at service time a nil cipher implies the
	// schema has no Sensitive items and encryption is never needed.
	cipher *dbkit.Cipher

	// cache is the process-local row cache; poller and subscriber share it
	// through invalidate, Set through put and invalidate.
	cache *valueCache

	// watchers is the Watch registry.
	watchers *watchers

	// pollInterval is the anti-loss poller's cadence; zero disables the
	// poller (tests use this or a short interval).
	pollInterval time.Duration

	// pollStop closes to stop the poller; pollDone signals the poller has
	// exited. Both are nil when the poller never started.
	pollStop chan struct{}
	pollDone chan struct{}

	// pollMu guards poller state: the watermark Refresh maintains and the
	// one-time stop.
	pollMu sync.Mutex

	// watermark is the newest updated_at Refresh has seen. Rows newer than
	// it are re-read next time; only the poller touches it, so it lives
	// behind pollMu.
	watermark time.Time

	// afterRefreshLock, when non-nil, is called synchronously by Refresh
	// immediately after it acquires pollMu and before it does any work. It
	// exists solely so a test can force the exact interleaving the
	// Close/poller deadlock this package's history records needs -- a
	// poller-triggered Refresh call provably holding pollMu at the moment
	// Close is invoked -- deterministically rather than by timing alone.
	// See TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh.
	// Nil on every production path.
	afterRefreshLock func()
}

// now is time.Now, indirected so tests can pin time if they ever need to;
// nothing in the module mutates it.
var now = time.Now

// Get resolves key to its effective value: the tenant row when the context
// carries a tenant and such a row exists, else the system row, else the
// schema default (docs/internal/11-cross-cutting.md's scope fallback). The
// returned Value carries the decoded typed value and the scope tier it was
// resolved at.
//
// Get serves ConfigItem keys and FeatureFlag keys alike -- a flag is a bool
// item -- but only the flag's plain effective value: whether a flag counts
// as "enabled" (its dependencies all enabled) is IsEnabled's question.
func (s *Service) Get(ctx context.Context, key string) (Value, error) {
	item, ok := s.schema.lookup(key)
	if !ok {
		return Value{}, ErrUnknownKey.WithParam("key", key)
	}
	canonical, scope, err := s.resolve(ctx, item)
	if err != nil {
		return Value{}, err
	}
	data, err := decodeValue(item.typ, canonical)
	if err != nil {
		return Value{}, ErrStorage.WithCause(err)
	}
	return Value{Data: data, Scope: scope}, nil
}

// GetTyped returns the effective value of key typed as T. T must be the Go
// kind the item's declared Type serves: string, int64, bool or
// time.Duration. An int item is served as int64, so GetTyped[int] fails
// with ErrTypedValueMismatch and GetTyped[int64] is the sanctioned read;
// any other mismatch fails the same way. Everything Get enforces (scope
// fallback, unknown key, unset item) applies unchanged.
func GetTyped[T any](svc *Service, ctx context.Context, key string) (T, error) {
	var out T
	v, err := svc.Get(ctx, key)
	if err != nil {
		return out, err
	}
	switch dst := any(&out).(type) {
	case *string:
		typed, ok := v.Data.(string)
		if !ok {
			return out, ErrTypedValueMismatch.WithParam("key", key)
		}
		*dst = typed
	case *int64:
		typed, ok := v.Data.(int64)
		if !ok {
			return out, ErrTypedValueMismatch.WithParam("key", key)
		}
		*dst = typed
	case *bool:
		typed, ok := v.Data.(bool)
		if !ok {
			return out, ErrTypedValueMismatch.WithParam("key", key)
		}
		*dst = typed
	case *time.Duration:
		typed, ok := v.Data.(time.Duration)
		if !ok {
			return out, ErrTypedValueMismatch.WithParam("key", key)
		}
		*dst = typed
	default:
		return out, ErrTypedValueMismatch.WithParam("key", key)
	}
	return out, nil
}

// Set writes v.Data as the value of key at scope. The value is validated
// against the key's schema entry (declared Type, declared Min/Max bounds)
// before anything is stored, the row's canonical form is encrypted when
// the item is Sensitive, and every successful write publishes a
// config.item.changed event and updates the process-local cache. The
// scope's own guards apply: ScopeTenant takes the owning tenant from ctx
// (never from the caller), ScopeSystem requires ctx to carry an audited
// system context, and ScopeUser is refused outright (ErrUserScopeUnavailable).
//
// by must be non-empty -- the actor is what the row's updated_by column,
// the event and the future audit trail all attribute the write to.
func (s *Service) Set(ctx context.Context, scope Scope, key string, v Value, by Actor) error {
	item, ok := s.schema.lookup(key)
	if !ok {
		return ErrUnknownKey.WithParam("key", key)
	}
	if by == "" {
		return ErrActorRequired.WithParam("key", key)
	}
	if err := validateScope(scope); err != nil {
		return decorateKey(err, key)
	}

	// The tier's entitlement checks: a tenant-scoped write needs the owning
	// tenant from the context; a system-scoped write needs an audited
	// system context. Both fail closed -- there is no anonymous tier to
	// fall into, and the caller-supplied identifier route is refused by
	// construction (the tenant comes from ctx, the actor from by).
	tenantID := ""
	switch scope {
	case ScopeTenant:
		tenant, ok := pkgcore.TenantFromContext(ctx)
		if !ok {
			return ErrTenantScopeRequiresTenant.WithParam("key", key)
		}
		tenantID = string(tenant)
	case ScopeSystem:
		if _, ok := pkgcore.SystemReasonFromContext(ctx); !ok {
			return ErrSystemScopeRequiresSystemContext.WithParam("key", key)
		}
	}

	// Canonicalize and bound-check before touching the store: the write
	// must be provably storable before a row is created for it. The error
	// never echoes v.Data, which may be Sensitive.
	canonical, err := canonicalizeValue(item.typ, v.Data)
	if err != nil {
		return ErrInvalidValue.WithParam("key", key).WithCause(err)
	}
	if err = rangeViolation(item.typ, canonical, item.minCanonical, item.maxCanonical); err != nil {
		return ErrInvalidValue.WithParam("key", key).WithCause(err)
	}

	stored := canonical
	if item.sensitive {
		if s.cipher == nil {
			return ErrCipherRequired.WithParam("key", key)
		}
		sealed, sealErr := s.cipher.Encrypt([]byte(canonical))
		if sealErr != nil {
			return ErrStorage.WithCause(sealErr)
		}
		stored = base64.StdEncoding.EncodeToString(sealed)
	}

	// Read the current row for the event's OldValue and for the upsert's
	// conflict target, then write. The row read and the row write are two
	// statements, not one transaction: this module has no outbox, and a
	// torn read here can at worst misreport the event's OldValue -- the
	// row itself is the source of truth and the poller heals caches.
	// (See AGENTS.md's known limitations for the no-outbox stance.)
	changedAt := now()
	existing, err := s.st.get(ctx, scope, tenantID, key)
	if err != nil {
		return ErrStorage.WithCause(err)
	}
	oldCanonical := ""
	if existing != nil {
		oldCanonical = existing.Value
		if item.sensitive {
			if plain, decErr := s.decrypt(existing.Value); decErr == nil {
				oldCanonical = plain
			} else {
				// An undecryptable old value still lets the write proceed;
				// the event reports the redacted marker either way.
				oldCanonical = redactedMarker
			}
		}
	}
	r := row{
		Key:       key,
		Scope:     string(scope),
		TenantID:  tenantID,
		Value:     stored,
		UpdatedBy: string(by),
		UpdatedAt: changedAt,
	}
	if err := s.st.put(ctx, r); err != nil {
		return ErrStorage.WithCause(err)
	}

	// The local cache is updated before the event is published: whatever
	// happens next, this process must not keep serving the value it just
	// overwrote. The subscriber will invalidate again (idempotent) and fire
	// the watchers.
	s.cache.put(key, scope, pkgcore.TenantID(tenantID), canonical, changedAt)

	evt := pkgcore.Event{
		Type:     EventConfigItemChanged,
		TenantID: pkgcore.TenantID(tenantID),
		Payload: ItemChangedEvent{
			Key:       key,
			Scope:     scope,
			TenantID:  tenantID,
			Actor:     string(by),
			OldValue:  redactIf(item.sensitive, oldCanonical),
			NewValue:  redactIf(item.sensitive, canonical),
			Sensitive: item.sensitive,
			ChangedAt: changedAt,
		},
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		return ErrAuditPublishFailed.WithCause(err)
	}
	return nil
}

// Watch registers fn to be called whenever key's value changes, at
// whatever scope the change happened. fn receives the Value of the change
// as it happened at the change's own scope: a tenant-tier change delivers
// the new value with ScopeTenant, a system-tier change with ScopeSystem --
// not the caller's re-resolved effective value, which may differ from the
// event's scope (a system-tier change affects every tenant; a tenant-tier
// change affects one tenant's rows, so under a shared event stream a
// watcher may see another tenant's override of the same key go by). A
// watcher that needs the effective value under its own context re-reads
// through Get. For Sensitive keys fn receives a redacted Value
// (Redacted == true, Data == nil): the value itself never leaves the
// process on the bus.
//
// Delivery semantics mirror the bus: on the standalone in-memory bus,
// fn runs synchronously inside the publishing Set; under the distributed
// bus it runs asynchronously on event delivery, and a Set whose event is
// lost (before the poller heals caches) never fires fn -- the poller
// converges readers, not watchers (see AGENTS.md).
//
// Registering the same fn for the same key twice registers it twice; the
// return value reports only whether key is a declared schema key, so an
// undeclared key fails before a useless callback is installed.
func (s *Service) Watch(key string, fn func(Value)) error {
	if _, ok := s.schema.lookup(key); !ok {
		return ErrUnknownKey.WithParam("key", key)
	}
	s.watchers.add(key, fn)
	return nil
}

// IsEnabled reports whether the feature flag key is enabled for the tenant
// the context carries (or platform-wide when it carries none): the flag's
// effective bool value (tenant row, else system row, else the flag's
// declared Default) and every flag in its DependsOn chain, recursively.
// A flag whose effective value is true but whose dependency is disabled is
// NOT enabled -- that is the entire point of DependsOn ("must be enabled
// for this one to have an effect"). IsEnabled fails with ErrUnknownFlag
// for a key that is not a declared FeatureFlag; the dependency graph is
// acyclic by Attach-time proof, so the walk always terminates.
func (s *Service) IsEnabled(ctx context.Context, key string) (bool, error) {
	item, ok := s.schema.lookup(key)
	if !ok || !item.isFlag {
		return false, ErrUnknownFlag.WithParam("key", key)
	}
	seen := make(map[string]bool)
	var enabled func(k string) (bool, error)
	enabled = func(k string) (bool, error) {
		entry, ok := s.schema.lookup(k)
		if !ok || !entry.isFlag {
			return false, ErrUnknownFlag.WithParam("key", k)
		}
		// Path-based DFS: a flag is marked only for the duration of the
		// walk of its own subtree, so a dependency reachable through two
		// chains -- a legal diamond, admitted by Attach's detectFlagCycles
		// -- is visited once per chain without tripping the guard. A
		// genuine cycle re-enters a flag still on the current path and
		// still fails here; the guard is the redundant net for schemas
		// built behind Attach's back.
		if seen[k] {
			return false, ErrFeatureFlagDependencyCycle.WithParam("key", k)
		}
		seen[k] = true
		defer delete(seen, k)
		canonical, _, err := s.resolve(ctx, entry)
		if err != nil {
			return false, err
		}
		flagOn := canonical == "true"
		if !flagOn {
			return false, nil
		}
		for _, dep := range entry.flagDeps {
			on, err := enabled(dep)
			if err != nil {
				return false, err
			}
			if !on {
				return false, nil
			}
		}
		return true, nil
	}
	return enabled(key)
}

// EnabledFlags returns every declared feature flag that IsEnabled reports
// enabled for the context's tenant, sorted ascending by key. It is the
// runtime half of the "/api/system/features query" contract
// (docs/internal/11-cross-cutting.md): consumers ask "which features are
// on" rather than probing one flag at a time. The returned slice is never
// nil: an empty result must marshal as JSON's [] -- the wire shape the
// features endpoints document -- not as null.
func (s *Service) EnabledFlags(ctx context.Context) ([]string, error) {
	out := make([]string, 0)
	for _, item := range s.schema.items {
		if !item.isFlag {
			continue
		}
		on, err := s.IsEnabled(ctx, item.key)
		if err != nil {
			return nil, err
		}
		if on {
			out = append(out, item.key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PublicSnapshot renders the response body of the unauthenticated
// /api/config/public endpoint: every Public item's effective value for the
// context's tenant, decoded and typed for JSON, plus the enabled feature
// flag list. Sensitive items can never appear (pkgcore's declaration
// validation makes Sensitive and Public mutually exclusive), so the
// snapshot is safe to serve to anyone. The returned map is keyed by
// configuration key; JSON output sorts map keys, keeping responses
// deterministic.
func (s *Service) PublicSnapshot(ctx context.Context) (map[string]any, []string, error) {
	values := make(map[string]any)
	keys := make([]string, 0, len(s.schema.items))
	for _, item := range s.schema.items {
		if item.public {
			keys = append(keys, item.key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		item := s.schema.items[key]
		canonical, _, err := s.resolve(ctx, item)
		if err != nil {
			return nil, nil, err
		}
		data, err := decodeValue(item.typ, canonical)
		if err != nil {
			return nil, nil, ErrStorage.WithCause(err)
		}
		// A duration decodes to time.Duration, which JSON would render as
		// its int64 nanosecond count. The public wire serves the canonical
		// "1m30s" form instead -- the same text the admin console shows.
		if d, ok := data.(time.Duration); ok {
			data = d.String()
		}
		values[key] = data
	}
	features, err := s.EnabledFlags(ctx)
	if err != nil {
		return nil, nil, err
	}
	return values, features, nil
}

// resolve walks the scope fallback for item and returns the effective
// canonical value and the scope tier it resolved at (the zero Scope for a
// schema default). The walk is cache-first for every tier: rows are read
// through the process-local cache, and a cache miss consults the store and
// populates the cache. Rows for tiers the context does not entitle the
// caller to are never consulted -- a context without a tenant skips the
// tenant tier entirely.
func (s *Service) resolve(ctx context.Context, item *schemaItem) (string, Scope, error) {
	if tenant, ok := pkgcore.TenantFromContext(ctx); ok {
		canonical, found, err := s.resolveRow(ctx, item, ScopeTenant, tenant)
		if err != nil {
			return "", "", err
		}
		if found {
			return canonical, ScopeTenant, nil
		}
	}
	canonical, found, err := s.resolveRow(ctx, item, ScopeSystem, "")
	if err != nil {
		return "", "", err
	}
	if found {
		return canonical, ScopeSystem, nil
	}
	if !item.hasDefault {
		return "", "", ErrItemUnset.WithParam("key", item.key)
	}
	return item.defaultCanonical, "", nil
}

// resolveRow returns one exact (scope, tenant) row's canonical value, or
// found == false when no such row exists. Rows are served from the cache
// when present; a miss reads the store and populates the cache (Sensitive
// rows are decrypted on this read -- the cache holds the plaintext
// canonical form, see valueCache's doc comment).
func (s *Service) resolveRow(ctx context.Context, item *schemaItem, scope Scope, tenant pkgcore.TenantID) (string, bool, error) {
	if entry, ok := s.cache.get(item.key, scope, tenant); ok {
		return entry.canonical, true, nil
	}
	r, err := s.st.get(ctx, scope, string(tenant), item.key)
	if err != nil {
		return "", false, ErrStorage.WithCause(err)
	}
	if r == nil {
		return "", false, nil
	}
	canonical := r.Value
	if item.sensitive {
		plain, err := s.decrypt(r.Value)
		if err != nil {
			return "", false, ErrStorage.WithCause(err)
		}
		canonical = plain
	}
	s.cache.put(item.key, scope, tenant, canonical, r.UpdatedAt)
	return canonical, true, nil
}

// decrypt unseals one stored Sensitive value: base64 then AES-GCM. The
// cipher is guaranteed non-nil when the schema has Sensitive items (Attach
// refused otherwise); a nil cipher reaching here is a schema that has no
// Sensitive items, which cannot happen for the caller's item.
func (s *Service) decrypt(stored string) (string, error) {
	if s.cipher == nil {
		return "", ErrCipherRequired
	}
	sealed, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", fmt.Errorf("config: stored sensitive value is not valid base64: %w", err)
	}
	plain, err := s.cipher.Decrypt(sealed)
	if err != nil {
		return "", fmt.Errorf("config: decrypt stored sensitive value: %w", err)
	}
	return string(plain), nil
}

// Refresh re-reads every row updated since the last Refresh (or since the
// service attached, on the first call) and invalidates the cache entries
// they belong to, then advances the watermark. It is the manual pump of
// the anti-loss poller and its test seam: the poller calls it on every
// tick, a test (or an operator with a reason to converge caches now) calls
// it directly. It never fires watches -- event loss converges readers, not
// watchers (see Watch's doc comment). Rows are not decrypted here: the
// poller needs only each row's cache address, never its content.
func (s *Service) Refresh(ctx context.Context) error {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	if s.afterRefreshLock != nil {
		s.afterRefreshLock()
	}
	rows, err := s.st.changedSince(ctx, s.watermark)
	if err != nil {
		return ErrStorage.WithCause(err)
	}
	for _, r := range rows {
		s.cache.invalidate(r.Key, Scope(r.Scope), pkgcore.TenantID(r.TenantID))
		if r.UpdatedAt.After(s.watermark) {
			s.watermark = r.UpdatedAt
		}
	}
	return nil
}

// Close stops the anti-loss poller, if one is running, and waits for it to
// exit. It is idempotent. The Service remains usable for direct reads and
// writes after Close -- Close only silences the background goroutine, so a
// host that shuts the poller down while request traffic drains does not
// lose the ability to serve.
//
// pollMu is held only long enough to capture and clear the poller-lifecycle
// fields, never across the wait: the poller goroutine calls Refresh on
// every tick, and Refresh itself locks pollMu, so a Close that stayed
// blocked on <-pollDone while still holding pollMu would deadlock against
// a tick that fired the instant before pollStop closed -- the poller
// could never finish that in-flight Refresh (it needs pollMu), and Close
// would never release pollMu until the poller finished. Releasing the
// lock before waiting lets that in-flight Refresh acquire and release
// pollMu normally, so the poller's next loop iteration observes the
// closed pollStop and exits.
func (s *Service) Close() error {
	s.pollMu.Lock()
	stop := s.pollStop
	done := s.pollDone
	if stop == nil {
		s.pollMu.Unlock()
		return nil
	}
	s.pollStop = nil
	s.pollDone = nil
	s.pollMu.Unlock()

	close(stop)
	<-done
	return nil
}

// startPoller launches the anti-loss poller goroutine: every pollInterval
// it calls Refresh against a timeout-bounded context. A Refresh error is
// not fatal -- the poller retries on the next tick. Nothing here can log
// (the module takes no logger), so the failure mode is silent-by-design
// with the documented consequence that a cache the poller cannot reach
// stays stale until it can; a host that needs to observe poller health can
// call Refresh itself through the same code path and surface the error.
func (s *Service) startPoller() {
	if s.pollInterval <= 0 {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.pollStop = stop
	s.pollDone = done
	// The goroutine below closes over the local stop/done copies, never
	// the s.pollStop/s.pollDone fields themselves: Close clears those
	// fields to nil (under pollMu) before this goroutine is guaranteed to
	// have exited, and reading them directly here -- on every loop
	// iteration's select, and in the deferred close -- would race against
	// that write.
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), s.pollInterval)
				_ = s.Refresh(ctx)
				cancel()
			}
		}
	}()
}

// onItemChanged is the subscriber the Service installs at Attach for
// EventConfigItemChanged. It invalidates the cache entry of the changed
// row -- wherever the change happened, this process re-reads on next
// access -- and fires the key's watchers with the change's own scope,
// whatever that scope was: a system-tier change is platform-wide news,
// a tenant-tier change is news for whoever cares about that key (even if,
// under a shared event stream, the change belongs to another tenant's
// row -- the watcher's contract is "the key changed", not "your effective
// value changed"; see Watch). On the in-memory bus this runs
// synchronously inside the publishing Set, so watchers fire before Set
// returns; on the distributed bus it runs on delivery.
//
// The payload is recovered with itemChangedFromWire: the in-memory bus
// delivers the concrete ItemChangedEvent, while a remote delivery from
// pkgcore's distributed bus arrives as the JSON-decoded map whose shape
// itemChangedFromJSONMap reads (without it, every cross-replica change
// would be dropped here and the anti-loss poller would be the only thing
// keeping replicas converged -- the event path is the primary one).
//
// The handler never returns an error on purpose: a returning error would
// bubble into the publisher's Publish call and misreport a remote Set as
// failed (ErrAuditPublishFailed) when the only problem was this process's
// own payload handling. The cache invalidation -- the part that must not
// fail -- happens before anything can. A payload of neither shape is
// ignored by construction, with the invalidation already done.
func (s *Service) onItemChanged(_ context.Context, evt pkgcore.Event) error {
	payload, ok := itemChangedFromWire(evt.Payload)
	if !ok {
		return nil
	}
	s.cache.invalidate(payload.Key, payload.Scope, pkgcore.TenantID(payload.TenantID))
	// A Sensitive item's change delivers a redacted Value: the canonical
	// form on the bus is the marker, and the real value never leaves this
	// process (see events.go).
	value := Value{Scope: payload.Scope, Redacted: payload.Sensitive}
	if !payload.Sensitive {
		if item, ok := s.schema.lookup(payload.Key); ok {
			// The NewValue was canonicalized by the publisher, so a decode
			// failure here is corruption, not a semantic case; a watcher
			// still fires with Data == nil rather than being dropped, and
			// the cache is already invalidated either way.
			if data, err := decodeValue(item.typ, payload.NewValue); err == nil {
				value.Data = data
			}
		}
	}
	s.watchers.fire(payload.Key, value)
	return nil
}

// decorateKey wraps an error that validateScope already produced with the
// offending key, when the error is an *apperr.Error that accepts params.
func decorateKey(err error, key string) error {
	appErr, ok := apperr.As(err)
	if !ok {
		return err
	}
	return appErr.WithParam("key", key)
}
