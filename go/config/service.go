package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// DefaultPollInterval is how often the anti-loss poller re-reads
// recently-updated configs rows when the host does not configure one. It is
// a documented package constant (the option is WithPollInterval): the
// poller is the net under the event bus, so its cadence only bounds how
// long a lost event can leave a stale cache -- the poll interval itself is
// deliberately long enough to be a no-op in the common case where no event
// was ever lost.
const DefaultPollInterval = 30 * time.Second

// fullReconcileEvery is how many Refresh cycles pass between one full
// cache reconciliation, layered on top of the ordinary incremental
// changedSince(watermark) sweep every cycle already performs.
//
// The incremental sweep alone has a permanent blind spot: watermark only
// ever advances forward, so a row whose UpdatedAt lands behind it --
// because the config.item.changed event for that write was lost (the
// event bus's own best-effort delivery) and the writer's own clock
// (service.go's now(), an application-supplied timestamp, never a
// database-generated one) was behind the clock that produced the
// watermark's current value -- can never be selected by
// "WHERE updated_at >= watermark" again, on any future cycle, however many.
// A cross-replica clock skew, or a single writer's clock stepping
// backward under an NTP correction, both genuinely produce exactly this.
//
// Rather than a real full-table diff (which would need to track every
// currently-cached key against a fresh store scan), Refresh's periodic
// reconciliation takes the simpler, equally effective route: it evicts
// every cache entry outright (valueCache.invalidateAll), so the very next
// read of any key -- stuck row included -- falls through to the store and
// observes its true current value, independent of where the watermark
// sits. The cost is a burst of cache misses every fullReconcileEvery
// cycles rather than none; at the default 30s poll interval that is one
// burst roughly every 10 minutes, far less frequent than the normal
// incremental poll. The accepted residual of the tradeoff: a stuck row
// still serves its stale cached value for up to fullReconcileEvery cycles,
// never longer.
const fullReconcileEvery = 20

// Service is the runtime face of the config module: the schema-driven,
// scope-resolved, cached, event-invalidated configuration store a host
// reads and writes through. A Service is obtained from (*Module).Attach,
// which freezes the schema snapshot from the booted registry and wires the
// store, the bus subscription and the poller. Until Attach runs, no Service
// exists (see ErrServiceNotAttached for what a route served in that window
// reports).
//
// The runtime surface: Get resolves a key across scopes, from tenant
// override down to system row and schema default, Set validates against the
// schema and writes the configs row, every Set
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

	// kv is the registry's KVStore, captured at Attach: the backend the two
	// pre-auth endpoints' per-address rate-limit budget is counted in
	// (ratelimit.go). Nil only for a hand-built, zero-value
	// *pkgcore.Registry (pkgcore.NewRegistry requires a store), and the
	// check fails closed on that rather than skipping itself.
	kv pkgcore.KVStore

	// cipher is the host's cipher, nil when the host injected
	// none. Attach refuses a schema containing Sensitive items without one
	// (ErrCipherRequired), so at service time a nil cipher implies the
	// schema has no Sensitive items and encryption is never needed.
	cipher *dbkit.Cipher

	// cache is the process-local cache of rows and confirmed absences;
	// poller and subscriber share it through invalidate, Set through put
	// and invalidate.
	cache *valueCache

	// watchers is the Watch registry.
	watchers *watchers

	// pollInterval is the anti-loss poller's cadence; zero disables the
	// poller (tests use this or a short interval).
	pollInterval time.Duration

	// pollStop closes to request the poller stop; the first Close call
	// closes it and clears the field. pollDone is closed by the poller
	// goroutine when it has actually exited and is deliberately never
	// cleared: it is the terminal signal every Close caller -- not just
	// the one that closed pollStop -- waits on before returning (see
	// Close's doc comment). Both are nil when the poller never started.
	pollStop chan struct{}
	pollDone chan struct{}

	// pollMu guards poller state: the watermark Refresh maintains and the
	// one-time stop.
	pollMu sync.Mutex

	// watermark is the newest updated_at Refresh has seen. Rows newer than
	// it are re-read next time; only the poller touches it, so it lives
	// behind pollMu.
	watermark time.Time

	// refreshCycles counts every completed Refresh call (poller-triggered
	// or manual alike); Refresh uses it, modulo fullReconcileEvery, to
	// decide when a cycle also performs a full cache reconciliation. Lives
	// behind pollMu exactly like watermark.
	refreshCycles uint64

	// afterRefreshLock, when non-nil, is called synchronously by Refresh
	// immediately after it acquires pollMu and before it does any work. It
	// exists solely so a test can force, deterministically rather than by
	// timing alone, the exact interleaving the Close/poller deadlock guard
	// protects against: a poller-triggered Refresh call provably holding
	// pollMu at the moment Close is invoked. See
	// TestService_Close_DoesNotDeadlockAgainstAnInFlightPollerRefresh. Nil on
	// every production path.
	afterRefreshLock func()
}

// now is time.Now, indirected so tests can pin time if they ever need to;
// nothing in the module mutates it.
var now = time.Now

// Get resolves key to its effective value: the tenant row when the context
// carries a tenant and such a row exists, else the system row, else the
// schema default (the scope fallback). The returned Value carries the
// decoded typed value and the scope tier it was resolved at.
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
// the event and the audit row the compliance module persists for the
// set (when a host composes it) all attribute the write to.
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
// converges readers, not watchers.
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
// enabled for the context's tenant, sorted ascending by key. It serves the
// PathSystemFeatures query contract: consumers ask "which features are on"
// rather than probing one flag at a time. The returned slice is never nil:
// an empty result must marshal as JSON's [] -- the wire shape the features
// endpoint documents -- not as null.
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
// PathPublic endpoint: every Public item's effective value for the
// context's tenant, decoded and typed for JSON, plus the enabled feature
// flag list. Sensitive items can never appear (pkgcore's declaration
// validation makes Sensitive and Public mutually exclusive), so the
// snapshot is safe to serve to anyone. An item the resolve walk cannot
// serve is omitted from the snapshot, never a reason to fail the response:
// that covers an item with no value anywhere -- no row at any reachable
// scope and no declared Default, a legal declaration whose module serves
// no value until one is set -- and an item whose stored row fails to
// decode under its declared type, which is skipped with a Warn naming it.
// The returned map is keyed by configuration key; JSON output sorts map
// keys, keeping responses deterministic.
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
		canonical, scope, err := s.resolve(ctx, item)
		if err != nil {
			// An item the resolve walk cannot serve is skipped -- that key
			// absent from the snapshot -- rather than failing the whole
			// response. The endpoint's contract is the platform-defaults
			// fallback, never an error: a module declaring a Public
			// item without a Default (legal: "the module serves no value
			// until one is set") must not take the pre-auth login surface
			// down for every tenant while ops has not written the row yet.
			// Any other resolve failure is genuine -- the store itself
			// refusing the read -- and still fails the response.
			if apperr.HasCode(err, ErrItemUnset.Code) {
				continue
			}
			return nil, nil, err
		}
		data, err := decodeValue(item.typ, canonical)
		if err != nil {
			// A stored row whose canonical text does not decode under the
			// item's declared type -- a row a buggy or older writer left
			// behind, or one edited outside the module -- is the sibling of
			// the unset case above: the bad data belongs to this one item,
			// so this one item is skipped, never the whole response. One
			// corrupt row taking the pre-auth login surface down for every
			// tenant would be the same outage the unset branch exists to
			// prevent, reached one step later in the row's life. The skip
			// is warned about, naming the item, so ops can find and fix the
			// row; a caller that must see the corruption reads the item
			// through Get, which still reports it.
			obs.FromContext(ctx).Warn("config: skipping an item whose stored value cannot be decoded as its declared type", "item", item.key, "scope", string(scope))
			continue
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
// populates the cache with the row it found -- or a confirmed-absence
// sentinel when it found none, so an unset key's fallback is served from
// the cache on the next read instead of querying every tier again. Rows
// for tiers the context does not entitle the caller to are never consulted
// -- a context without a tenant skips the tenant tier entirely.
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
// canonical form, see valueCache's doc comment). A no-row answer is
// cached too, as an absence sentinel, so a repeated read of an unset key
// does not re-consult the store (see valueCache.putMissing).
//
// The read-through backfill is generation-guarded: the cache's mutation
// generation is captured before the store read, and the backfill (through
// valueCache.putIfUnchanged for a row, putMissing for a confirmed absence)
// is dropped when any cache mutation landed while the read was in flight --
// a concurrent Set's own put or its invalidate, a poller sweep, a remote
// config.item.changed. Without the guard, a backfill whose store read
// completed before a concurrent write could land after that write's
// invalidate, planting the pre-write value -- or a pre-write absence -- in
// the cache until the next invalidation of the key or the periodic full
// reconciliation evicted it.
func (s *Service) resolveRow(ctx context.Context, item *schemaItem, scope Scope, tenant pkgcore.TenantID) (string, bool, error) {
	if entry, ok := s.cache.get(item.key, scope, tenant); ok {
		if entry.missing {
			return "", false, nil
		}
		return entry.canonical, true, nil
	}
	generation := s.cache.generation()
	r, err := s.st.get(ctx, scope, string(tenant), item.key)
	if err != nil {
		return "", false, ErrStorage.WithCause(err)
	}
	if r == nil {
		// No row exists at this tier: cache the confirmed absence so the
		// next read of the unset key is served from the cache like a
		// present one. The sentinel is generation-guarded exactly like the
		// positive backfill below, and a later Set that creates the row
		// lands its own put at this same triple (see valueCache.putMissing).
		s.cache.putMissing(item.key, scope, tenant, generation)
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
	s.cache.putIfUnchanged(item.key, scope, tenant, canonical, r.UpdatedAt, generation)
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
//
// Every fullReconcileEvery-th call also evicts the entire cache (see that
// constant's own doc comment): the incremental sweep above can never
// recover a row whose UpdatedAt landed behind an already-advanced
// watermark, and the periodic full reconciliation is what bounds how long
// that rare case can leave a cache entry stale, independent of the
// watermark's own position.
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
	s.refreshCycles++
	if s.refreshCycles%fullReconcileEvery == 0 {
		s.cache.invalidateAll()
	}
	return nil
}

// Close stops the anti-loss poller, if one is running, and waits for it to
// exit. It is idempotent. The Service remains usable for direct reads and
// writes after Close -- Close only silences the background goroutine, so a
// host that shuts the poller down while request traffic drains does not
// lose the ability to serve.
//
// pollMu is held only long enough to request the stop and read the
// poller-lifecycle fields, never across the wait: the poller goroutine
// calls Refresh on every tick, and Refresh itself locks pollMu, so a Close
// that stayed blocked on <-pollDone while still holding pollMu would
// deadlock against a tick that fired the instant before pollStop closed --
// the poller could never finish that in-flight Refresh (it needs pollMu),
// and Close would never release pollMu until the poller finished.
// Releasing the lock before waiting lets that in-flight Refresh acquire
// and release pollMu normally, so the poller's next loop iteration
// observes the closed pollStop and exits.
//
// Every caller waits on the same terminal signal, not just the one that
// closed pollStop: pollDone is deliberately never cleared once the poller
// has started, so a Close arriving after a concurrent caller has already
// closed and cleared pollStop still finds the channel to wait on. "Close
// returned" therefore implies "the poller goroutine has exited" for every
// caller -- the guarantee a host drains against before it tears down
// whatever the poller reads from -- while the deadlock fix above keeps
// each caller's wait free of pollMu.
func (s *Service) Close() error {
	s.pollMu.Lock()
	if s.pollStop != nil {
		// This caller requests the stop. Closing a channel never blocks,
		// and clearing the field under the same lock hold is what keeps a
		// concurrent caller from closing it a second time.
		close(s.pollStop)
		s.pollStop = nil
	}
	done := s.pollDone
	s.pollMu.Unlock()

	// done is nil only when the poller never started (pollInterval was
	// zero). Every other caller -- the one that closed pollStop and any
	// concurrent or later one -- waits here for the poller's exit; the
	// wait is outside pollMu per the deadlock analysis above.
	if done != nil {
		<-done
	}
	return nil
}

// startPoller launches the anti-loss poller goroutine: every pollInterval
// it calls Refresh against a timeout-bounded context. A Refresh error is
// not fatal -- the poller retries on the next tick. Nothing here logs:
// the poller runs against a bare background context carrying no logger,
// and the module takes no logger of its own (request-path code logs only
// through obs.FromContext's context logger, which a bare background
// context does not carry), so the failure mode is silent-by-design with
// the documented consequence that a cache the poller cannot reach stays
// stale until it can; a host that needs to observe poller health can call
// Refresh itself through the same code path and surface the error.
func (s *Service) startPoller() {
	if s.pollInterval <= 0 {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.pollStop = stop
	s.pollDone = done
	// The goroutine below closes over the local stop/done copies, never
	// the s.pollStop/s.pollDone fields themselves: the deferred close must
	// close the exact channel startPoller created, whatever a concurrent
	// Close later does to the fields, and reading s.pollStop directly on
	// every loop iteration would race against Close clearing it (under
	// pollMu) before this goroutine is guaranteed to have exited.
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
func (s *Service) onItemChanged(ctx context.Context, evt pkgcore.Event) error {
	payload, ok := itemChangedFromWire(evt.Payload)
	if !ok {
		return nil
	}
	s.cache.invalidate(payload.Key, payload.Scope, pkgcore.TenantID(payload.TenantID))
	// Whether the changed item is a Sensitive one is this process's own
	// schema's answer, never the payload's Sensitive flag's: the publisher
	// classifies from its own frozen schema copy, and during a rolling
	// upgrade the copies disagree -- an old copy that still believes a key
	// plaintext can publish the key's real value in the clear. The local
	// judgment decides the redaction (failing closed on a locally
	// Sensitive key whatever the remote claimed), and a disagreement is
	// Warned, because it means the replicas' schema snapshots have drifted
	// apart and an operator should know. It is deliberately not an error:
	// returning one would misreport a remote Set as failed (see the
	// handler's doc comment).
	item, known := s.schema.lookup(payload.Key)
	sensitive := known && item.sensitive
	if sensitive != payload.Sensitive {
		obs.FromContext(ctx).Warn("config: remote change-event sensitivity flag disagrees with the local schema; the local judgment governs", "item", payload.Key, "scope", string(payload.Scope), "payload_sensitive", payload.Sensitive, "schema_sensitive", sensitive)
	}
	// A Sensitive item's change delivers a redacted Value: the canonical
	// form on the bus is the marker, and the real value never leaves this
	// process (see events.go).
	value := Value{Scope: payload.Scope, Redacted: sensitive}
	if !sensitive && known {
		// The NewValue was canonicalized by the publisher, so a decode
		// failure here is corruption, not a semantic case; a watcher
		// still fires with Data == nil rather than being dropped, and
		// the cache is already invalidated either way.
		if data, err := decodeValue(item.typ, payload.NewValue); err == nil {
			value.Data = data
		}
	}
	s.watchers.fire(ctx, payload.Key, value)
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
