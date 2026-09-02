package pkgcore

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/pkgcore/locales"
)

// regTestModule is a minimal Module used to drive Bootstrap without pulling in
// a real business module.
type regTestModule struct {
	name     string
	deps     []string
	register func(reg *Registry) error
}

func (m regTestModule) Name() string         { return m.name }
func (m regTestModule) DependsOn() []string  { return m.deps }
func (m regTestModule) Migrations() embed.FS { return embed.FS{} }
func (m regTestModule) Locales() embed.FS    { return embed.FS{} }
func (m regTestModule) OpenAPISpec() []byte  { return nil }

func (m regTestModule) Register(reg *Registry) error {
	if m.register == nil {
		return nil
	}
	return m.register(reg)
}

// regTestRecorder builds a module that appends its own name to order when it
// registers, so that a test can assert the registration sequence.
func regTestRecorder(name string, deps []string, order *[]string) regTestModule {
	return regTestModule{
		name: name,
		deps: deps,
		register: func(*Registry) error {
			*order = append(*order, name)
			return nil
		},
	}
}

// regTestHandler is a comparable http.Handler, so a test can assert which
// handler ended up mounted at which path.
type regTestHandler struct{ id string }

func (regTestHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// regTestBus stands in for a distributed, broker-backed EventBus. It is a
// distinct type from the standalone in-memory bus on purpose, so that a test
// can assert which bus a registry or kernel actually ended up wired to.
type regTestBus struct {
	mu         sync.Mutex
	subscribed []string
	published  []Event
}

func newRegTestBus() *regTestBus { return &regTestBus{} }

func (b *regTestBus) Subscribe(eventType string, _ EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribed = append(b.subscribed, eventType)
}

func (b *regTestBus) Publish(_ context.Context, evt Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, evt)
	return nil
}

func (b *regTestBus) subscriptions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.subscribed)
}

// regTestKVStore stands in for a distributed, Redis-backed KVStore. Like
// regTestBus, it is a distinct type from the standalone in-memory store on
// purpose, so a test can assert which store a registry or kernel actually
// ended up wired to. Only Set is exercised by the tests below, so the
// remaining KVStore methods are trivial stubs rather than a full fake
// implementation.
type regTestKVStore struct {
	mu   sync.Mutex
	sets []string
}

func newRegTestKVStore() *regTestKVStore { return &regTestKVStore{} }

func (s *regTestKVStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (s *regTestKVStore) Set(_ context.Context, key string, _ []byte, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, key)
	return nil
}

func (s *regTestKVStore) Delete(context.Context, string) error { return nil }

func (s *regTestKVStore) IncrByFloat(context.Context, string, float64) (float64, error) {
	return 0, nil
}

func (s *regTestKVStore) CompareAndSwap(context.Context, string, []byte, []byte) (bool, error) {
	return false, nil
}

func (s *regTestKVStore) setKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sets)
}

func TestNewRegistry_WiresEveryRegistrar(t *testing.T) {
	bus := NewMemoryEventBus()
	kv := NewMemoryKVStore()
	reg := NewRegistry(bus, kv, NewConsoleMailer())

	if reg.Routes == nil {
		t.Error("Routes registrar is nil")
	}
	if reg.Config == nil {
		t.Error("Config registrar is nil")
	}
	if reg.Features == nil {
		t.Error("Features registrar is nil")
	}
	if reg.Permissions == nil {
		t.Error("Permissions registrar is nil")
	}
	if reg.Jobs == nil {
		t.Error("Jobs registrar is nil")
	}
	if reg.Notifications == nil {
		t.Error("Notifications registrar is nil")
	}
	if reg.Events == nil {
		t.Error("Events registrar is nil")
	}
	if reg.AuditActions == nil {
		t.Error("AuditActions registrar is nil")
	}
	if reg.EventBus() != bus {
		t.Errorf("EventBus() = %v, want the bus NewRegistry was given", reg.EventBus())
	}
	if reg.KVStore() != kv {
		t.Errorf("KVStore() = %v, want the store NewRegistry was given", reg.KVStore())
	}
}

func TestRouteRegistrar_Mount_RecordsRoutesInOrder(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())

	billing := regTestHandler{id: "billing"}
	org := regTestHandler{id: "org"}
	reg.Routes.Mount("/api/v1/billing", billing)
	reg.Routes.Mount("/api/v1/org", org)

	routes := reg.Routes.Routes()
	if len(routes) != 2 {
		t.Fatalf("Routes() returned %d routes, want 2", len(routes))
	}

	want := []MountedRoute{
		{Path: "/api/v1/billing", Handler: billing},
		{Path: "/api/v1/org", Handler: org},
	}
	for i, w := range want {
		if routes[i].Path != w.Path {
			t.Errorf("route %d path = %q, want %q", i, routes[i].Path, w.Path)
		}
		if routes[i].Handler != w.Handler {
			t.Errorf("route %d handler = %v, want %v", i, routes[i].Handler, w.Handler)
		}
	}
}

func TestRouteRegistrar_Routes_ReturnsCopy(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	reg.Routes.Mount("/api/v1/billing", regTestHandler{id: "billing"})

	mutated := reg.Routes.Routes()
	mutated[0].Path = "/hijacked"

	if got := reg.Routes.Routes()[0].Path; got != "/api/v1/billing" {
		t.Errorf("mutating the returned slice changed the registry: path = %q, want %q", got, "/api/v1/billing")
	}
}

func TestConfigRegistrar_Add_DuplicateKeyReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		first   []ConfigItem
		second  []ConfigItem
		wantErr bool
		wantKey string
	}{
		{
			name:    "distinct keys across calls are accepted",
			first:   []ConfigItem{{Key: "billing.retry_limit", Type: "int", Default: 3}},
			second:  []ConfigItem{{Key: "org.max_members", Type: "int", Default: 50}},
			wantErr: false,
		},
		{
			name:    "same key registered by a later module is rejected",
			first:   []ConfigItem{{Key: "billing.retry_limit", Type: "int", Default: 3}},
			second:  []ConfigItem{{Key: "billing.retry_limit", Type: "int", Default: 5}},
			wantErr: true,
			wantKey: "billing.retry_limit",
		},
		{
			name:  "same key repeated inside one call is rejected",
			first: nil,
			second: []ConfigItem{
				{Key: "billing.currency", Type: "string", Default: "CNY"},
				{Key: "billing.currency", Type: "string", Default: "USD"},
			},
			wantErr: true,
			wantKey: "billing.currency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			if len(tt.first) > 0 {
				if err := reg.Config.Add(tt.first...); err != nil {
					t.Fatalf("first Add() error = %v, want nil", err)
				}
			}

			err := reg.Config.Add(tt.second...)
			if tt.wantErr {
				if !errors.Is(err, ErrDuplicateConfigKey) {
					t.Fatalf("Add() error = %v, want it to wrap ErrDuplicateConfigKey", err)
				}
				if !strings.Contains(err.Error(), tt.wantKey) {
					t.Errorf("Add() error = %q, want it to name the key %q", err, tt.wantKey)
				}
				// A rejected call must register nothing from that call.
				if got := len(reg.Config.Items()); got != len(tt.first) {
					t.Errorf("after a rejected Add() there are %d items, want %d", got, len(tt.first))
				}
				return
			}
			if err != nil {
				t.Fatalf("Add() error = %v, want nil", err)
			}
			if got := len(reg.Config.Items()); got != len(tt.first)+len(tt.second) {
				t.Errorf("Items() returned %d items, want %d", got, len(tt.first)+len(tt.second))
			}
		})
	}
}

func TestConfigRegistrar_Items_PreservesDeclaration(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	want := ConfigItem{
		Key:         "billing.api_key",
		Type:        "string",
		Default:     "",
		Sensitive:   true,
		Description: "Payment provider API key.",
	}

	if err := reg.Config.Add(want); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}

	items := reg.Config.Items()
	if len(items) != 1 {
		t.Fatalf("Items() returned %d items, want 1", len(items))
	}
	if items[0] != want {
		t.Errorf("Items()[0] = %+v, want %+v", items[0], want)
	}
}

func TestPermissionRegistrar_Add_DuplicateAcrossModulesReturnsError(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())

	// Two modules register through the same shared Registry, as Bootstrap does.
	billing := regTestModule{
		name: "billing",
		register: func(reg *Registry) error {
			return reg.Permissions.Add("billing:read", "billing:manage")
		},
	}
	// A copy-paste mistake: the org module claims a billing permission.
	org := regTestModule{
		name: "org",
		deps: []string{"billing"},
		register: func(reg *Registry) error {
			return reg.Permissions.Add("org:read", "billing:manage")
		},
	}

	if err := billing.Register(reg); err != nil {
		t.Fatalf("billing.Register() error = %v, want nil", err)
	}

	err := org.Register(reg)
	if !errors.Is(err, ErrDuplicatePermission) {
		t.Fatalf("org.Register() error = %v, want it to wrap ErrDuplicatePermission", err)
	}
	if !strings.Contains(err.Error(), "billing:manage") {
		t.Errorf("error = %q, want it to name the duplicated permission %q", err, "billing:manage")
	}

	// The rejected call must not have registered its other permission either.
	got := reg.Permissions.Permissions()
	want := []string{"billing:manage", "billing:read"}
	if len(got) != len(want) {
		t.Fatalf("Permissions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Permissions() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestPermissionRegistrar_Permissions_IsSorted(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if err := reg.Permissions.Add("org:read", "billing:manage", "admin:impersonate"); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}

	got := reg.Permissions.Permissions()
	want := []string{"admin:impersonate", "billing:manage", "org:read"}
	if len(got) != len(want) {
		t.Fatalf("Permissions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Permissions() = %v, want %v", got, want)
		}
	}
}

func TestJobRegistrar_Handle_DuplicateJobTypeReturnsError(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	first := regTestHandler{id: "first"}
	second := regTestHandler{id: "second"}

	if err := reg.Jobs.Handle("billing.invoice.generate", first); err != nil {
		t.Fatalf("Handle() error = %v, want nil", err)
	}

	err := reg.Jobs.Handle("billing.invoice.generate", second)
	if !errors.Is(err, ErrDuplicateJobType) {
		t.Fatalf("Handle() error = %v, want it to wrap ErrDuplicateJobType", err)
	}
	if !strings.Contains(err.Error(), "billing.invoice.generate") {
		t.Errorf("error = %q, want it to name the job type", err)
	}

	// The first handler must survive the rejected registration.
	handlers := reg.Jobs.Handlers()
	if got := handlers["billing.invoice.generate"]; got != any(first) {
		t.Errorf("handler = %v, want the originally registered %v", got, first)
	}
}

func TestNotificationRegistrar_Add_DuplicateKeyReturnsError(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	paid := NotificationType{
		Key:             "billing.invoice_paid",
		Group:           "billing",
		DefaultChannels: []string{"email"},
		Unsubscribable:  true,
	}

	if err := reg.Notifications.Add(paid); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}

	err := reg.Notifications.Add(NotificationType{Key: "billing.invoice_paid", Group: "other"})
	if !errors.Is(err, ErrDuplicateNotificationType) {
		t.Fatalf("Add() error = %v, want it to wrap ErrDuplicateNotificationType", err)
	}

	types := reg.Notifications.Types()
	if len(types) != 1 {
		t.Fatalf("Types() returned %d types, want 1", len(types))
	}
	if types[0].Group != "billing" {
		t.Errorf("Types()[0].Group = %q, want the originally registered %q", types[0].Group, "billing")
	}
}

func TestAuditActionRegistrar_Add_DuplicateReturnsErrorAndSorts(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if err := reg.AuditActions.Add("org.member.removed", "billing.plan.changed"); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}

	if err := reg.AuditActions.Add("billing.plan.changed"); !errors.Is(err, ErrDuplicateAuditAction) {
		t.Fatalf("Add() error = %v, want it to wrap ErrDuplicateAuditAction", err)
	}

	got := reg.AuditActions.Actions()
	want := []string{"billing.plan.changed", "org.member.removed"}
	if len(got) != len(want) {
		t.Fatalf("Actions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Actions() = %v, want %v", got, want)
		}
	}
}

func TestEventRegistrar_Subscribe_IsBackedByTheRegistryEventBus(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())

	var mu sync.Mutex
	var received []Event
	reg.Events.Subscribe("org.member.invited", func(_ context.Context, evt Event) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, evt)
		return nil
	})

	// Several subscribers on one event type are expected, not a conflict.
	var secondCalls int
	reg.Events.Subscribe("org.member.invited", func(context.Context, Event) error {
		mu.Lock()
		defer mu.Unlock()
		secondCalls++
		return nil
	})

	want := Event{Type: "org.member.invited", TenantID: TenantID("tenant-1"), Payload: "payload"}
	if err := reg.EventBus().Publish(context.Background(), want); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("first subscriber received %d events, want 1", len(received))
	}
	if received[0].Type != want.Type || received[0].TenantID != want.TenantID {
		t.Errorf("received %+v, want %+v", received[0], want)
	}
	if secondCalls != 1 {
		t.Errorf("second subscriber called %d times, want 1", secondCalls)
	}
}

func TestFeatureRegistrar_Add_DuplicateKeyReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		first   []FeatureFlag
		second  []FeatureFlag
		wantErr bool
		wantKey string
	}{
		{
			name:    "distinct keys across calls are accepted",
			first:   []FeatureFlag{{Key: "billing.dunning", Default: true}},
			second:  []FeatureFlag{{Key: "org.workspaces"}},
			wantErr: false,
		},
		{
			// Two modules owning one flag leaves its default decided by
			// registration order, and the admin console renders the key twice.
			name:    "same key registered by a later module is rejected",
			first:   []FeatureFlag{{Key: "billing.dunning", Default: true, Description: "billing owns it"}},
			second:  []FeatureFlag{{Key: "billing.dunning", Default: false, Description: "org clobbers it"}},
			wantErr: true,
			wantKey: "billing.dunning",
		},
		{
			name:  "same key repeated inside one call is rejected",
			first: nil,
			second: []FeatureFlag{
				{Key: "ai.vision", Default: true},
				{Key: "ai.vision", Default: false},
			},
			wantErr: true,
			wantKey: "ai.vision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			if len(tt.first) > 0 {
				if err := reg.Features.Add(tt.first...); err != nil {
					t.Fatalf("first Add() error = %v, want nil", err)
				}
			}

			err := reg.Features.Add(tt.second...)
			if tt.wantErr {
				if !errors.Is(err, ErrDuplicateFeatureFlag) {
					t.Fatalf("Add() error = %v, want it to wrap ErrDuplicateFeatureFlag", err)
				}
				if !strings.Contains(err.Error(), tt.wantKey) {
					t.Errorf("Add() error = %q, want it to name the key %q", err, tt.wantKey)
				}
				// A rejected call must register nothing from that call.
				if got := len(reg.Features.Flags()); got != len(tt.first) {
					t.Errorf("after a rejected Add() there are %d flags, want %d", got, len(tt.first))
				}
				return
			}
			if err != nil {
				t.Fatalf("Add() error = %v, want nil", err)
			}
			if got := len(reg.Features.Flags()); got != len(tt.first)+len(tt.second) {
				t.Errorf("Flags() returned %d flags, want %d", got, len(tt.first)+len(tt.second))
			}
		})
	}
}

// TestBootstrap_DuplicateFeatureFlagAcrossModules_ReturnsError is the
// cross-module shape of the same bug: two modules each claiming one flag key
// used to bootstrap successfully with two contradictory defaults.
func TestBootstrap_DuplicateFeatureFlagAcrossModules_ReturnsError(t *testing.T) {
	billing := regTestModule{
		name: "billing",
		register: func(reg *Registry) error {
			return reg.Features.Add(FeatureFlag{Key: "billing.dunning", Default: true})
		},
	}
	org := regTestModule{
		name: "org",
		deps: []string{"billing"},
		register: func(reg *Registry) error {
			return reg.Features.Add(FeatureFlag{Key: "billing.dunning", Default: false})
		},
	}

	reg, err := NewKernel(DeploymentModeStandalone).Bootstrap(context.Background(), org, billing)
	if !errors.Is(err, ErrDuplicateFeatureFlag) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrDuplicateFeatureFlag", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if !strings.Contains(err.Error(), "org") {
		t.Errorf("error = %q, want it to name the module that registered the duplicate", err)
	}
}

func TestEventRegistrar_Publishes_RecordsTheDeclaredCatalog(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())

	want := []EventDecl{
		{Type: "billing.invoice.paid", PayloadType: "billing.InvoicePaid", Description: "An invoice was paid in full."},
		{Type: "billing.subscription.cancelled", PayloadType: "billing.SubscriptionCancelled", Description: "A subscription was cancelled."},
	}
	if err := reg.Events.Publishes(want...); err != nil {
		t.Fatalf("Publishes() error = %v, want nil", err)
	}

	got := reg.Events.Published()
	if len(got) != len(want) {
		t.Fatalf("Published() returned %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Published()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The catalog is what integration maps to its public schema, so a caller
	// must not be able to edit it through the returned slice.
	got[0].Type = "hijacked"
	if reg.Events.Published()[0].Type != want[0].Type {
		t.Error("mutating the returned slice changed the registry")
	}
}

func TestEventRegistrar_Publishes_DuplicateTypeReturnsError(t *testing.T) {
	tests := []struct {
		name     string
		first    []EventDecl
		second   []EventDecl
		wantErr  bool
		wantType string
	}{
		{
			name:    "distinct types across calls are accepted",
			first:   []EventDecl{{Type: "billing.invoice.paid"}},
			second:  []EventDecl{{Type: "org.member.invited"}},
			wantErr: false,
		},
		{
			// Exactly one module owns an event type: two publishers of one
			// type make the payload contract ambiguous for every subscriber.
			name:     "same type declared by a later module is rejected",
			first:    []EventDecl{{Type: "billing.invoice.paid", PayloadType: "billing.InvoicePaid"}},
			second:   []EventDecl{{Type: "billing.invoice.paid", PayloadType: "org.InvoicePaid"}},
			wantErr:  true,
			wantType: "billing.invoice.paid",
		},
		{
			name:  "same type repeated inside one call is rejected",
			first: nil,
			second: []EventDecl{
				{Type: "billing.invoice.paid"},
				{Type: "billing.invoice.paid"},
			},
			wantErr:  true,
			wantType: "billing.invoice.paid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			if len(tt.first) > 0 {
				if err := reg.Events.Publishes(tt.first...); err != nil {
					t.Fatalf("first Publishes() error = %v, want nil", err)
				}
			}

			err := reg.Events.Publishes(tt.second...)
			if tt.wantErr {
				if !errors.Is(err, ErrDuplicateEventType) {
					t.Fatalf("Publishes() error = %v, want it to wrap ErrDuplicateEventType", err)
				}
				if !strings.Contains(err.Error(), tt.wantType) {
					t.Errorf("Publishes() error = %q, want it to name the type %q", err, tt.wantType)
				}
				if got := len(reg.Events.Published()); got != len(tt.first) {
					t.Errorf("after a rejected Publishes() there are %d events, want %d", got, len(tt.first))
				}
				return
			}
			if err != nil {
				t.Fatalf("Publishes() error = %v, want nil", err)
			}
			if got := len(reg.Events.Published()); got != len(tt.first)+len(tt.second) {
				t.Errorf("Published() returned %d events, want %d", got, len(tt.first)+len(tt.second))
			}
		})
	}
}

// TestRegistry_EventBus_FollowsTheEventsRegistrar pins the invariant that a
// substituted Events registrar takes the bus with it. When the bus was stored
// separately, replacing Events left EventBus() pointing at the old bus, so
// publishers and subscribers ended up on different buses with no error.
func TestRegistry_EventBus_FollowsTheEventsRegistrar(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())

	distributed := newRegTestBus()
	reg.Events = &memoryEventRegistrar{bus: distributed, types: make(map[string]struct{})}

	if reg.EventBus() != distributed {
		t.Fatalf("EventBus() = %v, want the substituted registrar's bus", reg.EventBus())
	}

	reg.Events.Subscribe("billing.invoice.paid", func(context.Context, Event) error { return nil })
	got := reg.Events.Bus().(*regTestBus).subscriptions()
	if len(got) != 1 || got[0] != "billing.invoice.paid" {
		t.Errorf("substituted bus recorded subscriptions %v, want [billing.invoice.paid]", got)
	}
}

func TestRegistry_EventBus_ZeroValueRegistryReturnsNil(t *testing.T) {
	if bus := (&Registry{}).EventBus(); bus != nil {
		t.Errorf("EventBus() = %v, want nil for a registry with no Events registrar", bus)
	}
}

// TestRegistry_KVStore_ZeroValueRegistryReturnsNil mirrors
// TestRegistry_EventBus_ZeroValueRegistryReturnsNil. Unlike EventBus, KVStore
// is not derived from a registrar: kv is a plain field, so a zero-value
// Registry returns nil the same way any zero-value interface field does, with
// no explicit nil-registrar guard needed inside KVStore() itself.
func TestRegistry_KVStore_ZeroValueRegistryReturnsNil(t *testing.T) {
	if kv := (&Registry{}).KVStore(); kv != nil {
		t.Errorf("KVStore() = %v, want nil for a registry with no kv wired in", kv)
	}
}

// TestRegistry_Mailer_ZeroValueRegistryReturnsNil mirrors
// TestRegistry_KVStore_ZeroValueRegistryReturnsNil for the mail seam: mailer
// is a plain field for the same reason kv is, so the zero-value Registry
// answers nil the same way.
func TestRegistry_Mailer_ZeroValueRegistryReturnsNil(t *testing.T) {
	if mailer := (&Registry{}).Mailer(); mailer != nil {
		t.Errorf("Mailer() = %v, want nil for a registry with no mailer wired in", mailer)
	}
}

// TestRegistry_ObjectStore_ZeroValueRegistryReturnsNil mirrors
// TestRegistry_Mailer_ZeroValueRegistryReturnsNil for the object-store seam:
// objectStore is a plain field for the same reason mailer's is, so the
// zero-value Registry answers nil the same way.
func TestRegistry_ObjectStore_ZeroValueRegistryReturnsNil(t *testing.T) {
	if store := (&Registry{}).ObjectStore(); store != nil {
		t.Errorf("ObjectStore() = %v, want nil for a registry with no store wired in", store)
	}
}

// TestRegistry_ObjectStore_NewRegistryBuiltRegistryReturnsNil pins the
// accessor's contract that only a Bootstrap-built registry carries an object
// store. NewRegistry's three-argument signature predates the seam and cannot
// accept one, so a registry built by hand answers nil until Bootstrap fills
// the field in, before any module registration runs.
func TestRegistry_ObjectStore_NewRegistryBuiltRegistryReturnsNil(t *testing.T) {
	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if store := reg.ObjectStore(); store != nil {
		t.Errorf("ObjectStore() = %v, want nil until Bootstrap resolves one", store)
	}
}

func TestNewRegistry_NilBus_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRegistry(nil) did not panic, want a panic rather than a registry that drops every event")
		}
	}()

	NewRegistry(nil, NewMemoryKVStore(), NewConsoleMailer())
}

// TestNewRegistry_NilKVStore_Panics mirrors TestNewRegistry_NilBus_Panics for
// the key-value seam.
func TestNewRegistry_NilKVStore_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRegistry(nil) did not panic, want a panic rather than a registry whose KVStore() panics on first use")
		}
	}()

	NewRegistry(NewMemoryEventBus(), nil, NewConsoleMailer())
}

// TestNewRegistry_NilMailer_Panics mirrors TestNewRegistry_NilBus_Panics and
// TestNewRegistry_NilKVStore_Panics for the mail seam.
func TestNewRegistry_NilMailer_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRegistry(nil) did not panic, want a panic rather than a registry whose Mailer() accepts every mail and sends nothing")
		}
	}()

	NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), nil)
}

func TestValidateFeatureGraph(t *testing.T) {
	tests := []struct {
		name          string
		flags         []FeatureFlag
		wantErr       bool
		wantMentioned []string
	}{
		{
			name:    "no flags at all resolves",
			flags:   nil,
			wantErr: false,
		},
		{
			name: "flags without dependencies resolve",
			flags: []FeatureFlag{
				{Key: "billing.dunning"},
				{Key: "org.workspaces"},
			},
			wantErr: false,
		},
		{
			name: "dependency on a flag registered by another module resolves",
			flags: []FeatureFlag{
				{Key: "billing.dunning", DependsOn: []string{"org.workspaces"}},
				{Key: "org.workspaces"},
			},
			wantErr: false,
		},
		{
			name: "dependency on a flag registered later still resolves",
			flags: []FeatureFlag{
				{Key: "billing.dunning", DependsOn: []string{"billing.invoicing"}},
				{Key: "billing.invoicing"},
			},
			wantErr: false,
		},
		{
			name: "dependency on an unregistered flag is rejected",
			flags: []FeatureFlag{
				{Key: "billing.dunning", DependsOn: []string{"billing.invoicing"}},
			},
			wantErr:       true,
			wantMentioned: []string{"billing.dunning", "billing.invoicing"},
		},
		{
			name: "every unresolved dependency is reported",
			flags: []FeatureFlag{
				{Key: "billing.dunning", DependsOn: []string{"billing.invoicing", "org.seats"}},
				{Key: "ai.vision", DependsOn: []string{"ai.gateway"}},
			},
			wantErr:       true,
			wantMentioned: []string{"billing.invoicing", "org.seats", "ai.gateway"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
			if err := reg.Features.Add(tt.flags...); err != nil {
				t.Fatalf("Features.Add() error = %v, want nil", err)
			}

			err := ValidateFeatureGraph(reg)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateFeatureGraph() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUnresolvedFeatureDependency) {
				t.Fatalf("ValidateFeatureGraph() error = %v, want it to wrap ErrUnresolvedFeatureDependency", err)
			}
			for _, key := range tt.wantMentioned {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error = %q, want it to name %q", err, key)
				}
			}
		})
	}
}

func TestValidateFeatureGraph_UnwiredRegistry_ReturnsError(t *testing.T) {
	if err := ValidateFeatureGraph(nil); err == nil {
		t.Error("ValidateFeatureGraph(nil) error = nil, want an error")
	}
	if err := ValidateFeatureGraph(&Registry{}); err == nil {
		t.Error("ValidateFeatureGraph(&Registry{}) error = nil, want an error")
	}
}

func TestBootstrap_ValidChain_RegistersInDependencyOrder(t *testing.T) {
	tests := []struct {
		name    string
		modules func(order *[]string) []Module
		want    []string
	}{
		{
			name: "a linear chain registers leaf first",
			modules: func(order *[]string) []Module {
				return []Module{
					regTestRecorder("a", []string{"b"}, order),
					regTestRecorder("b", []string{"c"}, order),
					regTestRecorder("c", nil, order),
				}
			},
			want: []string{"c", "b", "a"},
		},
		{
			name: "input order does not change the result",
			modules: func(order *[]string) []Module {
				return []Module{
					regTestRecorder("c", nil, order),
					regTestRecorder("a", []string{"b"}, order),
					regTestRecorder("b", []string{"c"}, order),
				}
			},
			want: []string{"c", "b", "a"},
		},
		{
			name: "a diamond registers each module exactly once",
			modules: func(order *[]string) []Module {
				return []Module{
					regTestRecorder("app", []string{"left", "right"}, order),
					regTestRecorder("left", []string{"core"}, order),
					regTestRecorder("right", []string{"core"}, order),
					regTestRecorder("core", nil, order),
				}
			},
			want: []string{"core", "left", "right", "app"},
		},
		{
			name: "independent modules keep their input order",
			modules: func(order *[]string) []Module {
				return []Module{
					regTestRecorder("first", nil, order),
					regTestRecorder("second", nil, order),
				}
			},
			want: []string{"first", "second"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var order []string
			kernel := NewKernel(DeploymentModeStandalone)

			reg, err := kernel.Bootstrap(context.Background(), tt.modules(&order)...)
			if err != nil {
				t.Fatalf("Bootstrap() error = %v, want nil", err)
			}
			if reg == nil {
				t.Fatal("Bootstrap() returned a nil registry")
			}
			if len(order) != len(tt.want) {
				t.Fatalf("registration order = %v, want %v", order, tt.want)
			}
			for i := range tt.want {
				if order[i] != tt.want[i] {
					t.Fatalf("registration order = %v, want %v", order, tt.want)
				}
			}
		})
	}
}

// depChainModule is a minimal Module used only by the dependency-chain-order
// tests below. It is defined separately from regTestModule so these tests
// stay independent of that fixture's own assumptions.
type depChainModule struct {
	name  string
	deps  []string
	order *[]string
}

func (m depChainModule) Name() string         { return m.name }
func (m depChainModule) DependsOn() []string  { return m.deps }
func (m depChainModule) Migrations() embed.FS { return embed.FS{} }
func (m depChainModule) Locales() embed.FS    { return embed.FS{} }
func (m depChainModule) OpenAPISpec() []byte  { return nil }

// Register records the order in which the kernel drove the modules. Bootstrap
// registers sequentially, so no synchronisation is needed here.
func (m depChainModule) Register(_ *Registry) error {
	*m.order = append(*m.order, m.name)
	return nil
}

func depChainMod(name string, deps []string, order *[]string) depChainModule {
	return depChainModule{name: name, deps: deps, order: order}
}

// assertDependencyOrder checks the property the topological sort actually
// promises: every module appears after each of its declared dependencies, and
// each module is registered exactly once. Asserting the property rather than a
// hardcoded slice keeps the test honest for graphs with several valid orders.
func assertDependencyOrder(t *testing.T, order []string, deps map[string][]string) {
	t.Helper()

	position := make(map[string]int, len(order))
	for i, name := range order {
		if prev, dup := position[name]; dup {
			t.Errorf("module %q registered twice, at %d and %d", name, prev, i)
			continue
		}
		position[name] = i
	}
	if len(position) != len(deps) {
		t.Errorf("registered %d distinct modules, want %d (order: %v)", len(position), len(deps), order)
	}
	for name, on := range deps {
		self, ok := position[name]
		if !ok {
			t.Errorf("module %q was never registered (order: %v)", name, order)
			continue
		}
		for _, dep := range on {
			at, ok := position[dep]
			if !ok {
				t.Errorf("dependency %q of %q was never registered (order: %v)", dep, name, order)
				continue
			}
			if at > self {
				t.Errorf("module %q (position %d) was registered before its dependency %q (position %d); order: %v",
					name, self, dep, at, order)
			}
		}
	}
}

// TestBootstrap_DependencyChainsLongerThanOneHop confirms the topological sort
// follows a chain all the way down rather than only ordering a single pair.
// TestBootstrap_ValidChain_RegistersInDependencyOrder above covers a
// three-module chain; these cases push the chain to five modules, pass the
// modules in an order that is the exact reverse of the answer, and add a
// diamond where one module is reached by two different paths and so must
// still be registered exactly once.
func TestBootstrap_DependencyChainsLongerThanOneHop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		deps map[string][]string
		// input is the order the modules are handed to Bootstrap.
		input []string
		// want is the single correct order, or nil when several are valid and
		// only the ordering property is asserted.
		want []string
	}{
		{
			name:  "three modules chained a->b->c",
			deps:  map[string][]string{"a": {"b"}, "b": {"c"}, "c": nil},
			input: []string{"a", "b", "c"},
			want:  []string{"c", "b", "a"},
		},
		{
			name:  "five modules chained a->b->c->d->e",
			deps:  map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"d"}, "d": {"e"}, "e": nil},
			input: []string{"a", "b", "c", "d", "e"},
			want:  []string{"e", "d", "c", "b", "a"},
		},
		{
			name:  "a five module chain handed over in reverse still sorts",
			deps:  map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"d"}, "d": {"e"}, "e": nil},
			input: []string{"e", "d", "c", "b", "a"},
			want:  []string{"e", "d", "c", "b", "a"},
		},
		{
			name: "a transitive dependency two hops down is still ordered first",
			deps: map[string][]string{"top": {"mid"}, "mid": {"leaf"}, "leaf": nil, "loner": nil},
			// "loner" is unrelated, so it must not disturb the chain.
			input: []string{"loner", "top", "mid", "leaf"},
			want:  []string{"loner", "leaf", "mid", "top"},
		},
		{
			name: "a diamond registers the shared base exactly once",
			deps: map[string][]string{
				"app":   {"left", "right"},
				"left":  {"base"},
				"right": {"base"},
				"base":  nil,
			},
			input: []string{"app", "left", "right", "base"},
			// base -> left -> right -> app is the only order the input
			// ordering can produce, but the property check is what matters.
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var order []string
			modules := make([]Module, 0, len(tt.input))
			for _, name := range tt.input {
				modules = append(modules, depChainMod(name, tt.deps[name], &order))
			}

			reg, err := NewKernel(DeploymentModeStandalone).Bootstrap(context.Background(), modules...)
			if err != nil {
				t.Fatalf("Bootstrap returned an error: %v", err)
			}
			if reg == nil {
				t.Fatal("Bootstrap returned a nil registry with a nil error")
			}
			assertDependencyOrder(t, order, tt.deps)
			if tt.want != nil && !slices.Equal(order, tt.want) {
				t.Errorf("registration order = %v, want %v", order, tt.want)
			}
		})
	}
}

func TestBootstrap_DependencyCycle_ErrorNamesTheCycle(t *testing.T) {
	tests := []struct {
		name    string
		modules []Module
		want    []string
	}{
		{
			name: "two modules depending on each other",
			modules: []Module{
				regTestModule{name: "a", deps: []string{"b"}},
				regTestModule{name: "b", deps: []string{"a"}},
			},
			want: []string{"a", "b", "a"},
		},
		{
			name: "three modules forming a cycle",
			modules: []Module{
				regTestModule{name: "a", deps: []string{"b"}},
				regTestModule{name: "b", deps: []string{"c"}},
				regTestModule{name: "c", deps: []string{"a"}},
			},
			want: []string{"a", "b", "c", "a"},
		},
		{
			name: "a module depending on itself",
			modules: []Module{
				regTestModule{name: "a", deps: []string{"a"}},
			},
			want: []string{"a", "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kernel := NewKernel(DeploymentModeStandalone)

			reg, err := kernel.Bootstrap(context.Background(), tt.modules...)
			if !errors.Is(err, ErrDependencyCycle) {
				t.Fatalf("Bootstrap() error = %v, want it to wrap ErrDependencyCycle", err)
			}
			if reg != nil {
				t.Error("Bootstrap() returned a registry alongside the error, want nil")
			}

			wantChain := strings.Join(tt.want, " -> ")
			if !strings.Contains(err.Error(), wantChain) {
				t.Errorf("error = %q, want it to contain the cycle %q", err, wantChain)
			}
		})
	}
}

func TestBootstrap_DependencyNotInModuleList_ErrorNamesBothModules(t *testing.T) {
	kernel := NewKernel(DeploymentModeStandalone)

	reg, err := kernel.Bootstrap(context.Background(),
		regTestModule{name: "billing", deps: []string{"metering"}},
		regTestModule{name: "org"},
	)

	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrMissingDependency", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	for _, want := range []string{"billing", "metering"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

// TestBootstrap_DeepChainMissingTailIsReported makes sure the sort does not
// lose a broken edge simply because it sits several hops down a chain, which a
// sort that only inspected direct dependencies could miss.
func TestBootstrap_DeepChainMissingTailIsReported(t *testing.T) {
	t.Parallel()

	var order []string
	// d depends on "e", which is never handed to Bootstrap.
	modules := []Module{
		depChainMod("a", []string{"b"}, &order),
		depChainMod("b", []string{"c"}, &order),
		depChainMod("c", []string{"d"}, &order),
		depChainMod("d", []string{"e"}, &order),
	}

	reg, err := NewKernel(DeploymentModeStandalone).Bootstrap(context.Background(), modules...)
	if err == nil {
		t.Fatal("Bootstrap succeeded, want an error naming the missing dependency")
	}
	if reg != nil {
		t.Error("Bootstrap returned a non-nil registry alongside an error")
	}
	if len(order) != 0 {
		t.Errorf("modules were registered before the graph was validated: %v", order)
	}
}

func TestBootstrap_DuplicateModuleName_ReturnsError(t *testing.T) {
	kernel := NewKernel(DeploymentModeStandalone)

	_, err := kernel.Bootstrap(context.Background(),
		regTestModule{name: "billing"},
		regTestModule{name: "billing"},
	)

	if !errors.Is(err, ErrDuplicateModuleName) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrDuplicateModuleName", err)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("error = %q, want it to name %q", err, "billing")
	}
}

func TestBootstrap_RegisterFails_WrapsModuleNameAndStops(t *testing.T) {
	var order []string
	failure := errors.New("schema declaration is broken")

	failing := regTestModule{
		name: "billing",
		register: func(*Registry) error {
			order = append(order, "billing")
			return failure
		},
	}
	// "org" depends on "billing", so it registers strictly after the failure.
	later := regTestRecorder("org", []string{"billing"}, &order)

	kernel := NewKernel(DeploymentModeStandalone)
	reg, err := kernel.Bootstrap(context.Background(), later, failing)

	if !errors.Is(err, failure) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap the module error", err)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("error = %q, want it to name the failing module %q", err, "billing")
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 1 || order[0] != "billing" {
		t.Errorf("registration order = %v, want registration to stop after %q", order, "billing")
	}
}

func TestBootstrap_UnresolvedFeatureDependency_ReturnsError(t *testing.T) {
	// The flag graph only breaks once both modules have registered, which is
	// exactly why Bootstrap validates it at the end rather than inside Add.
	billing := regTestModule{
		name: "billing",
		register: func(reg *Registry) error {
			return reg.Features.Add(FeatureFlag{
				Key:       "billing.dunning",
				DependsOn: []string{"metering.usage"},
			})
		},
	}
	org := regTestModule{
		name: "org",
		register: func(reg *Registry) error {
			return reg.Features.Add(FeatureFlag{Key: "org.workspaces"})
		},
	}

	kernel := NewKernel(DeploymentModeStandalone)
	reg, err := kernel.Bootstrap(context.Background(), billing, org)

	if !errors.Is(err, ErrUnresolvedFeatureDependency) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrUnresolvedFeatureDependency", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if !strings.Contains(err.Error(), "metering.usage") {
		t.Errorf("error = %q, want it to name the unresolved flag %q", err, "metering.usage")
	}
}

func TestBootstrap_SharesOneRegistryAcrossModules(t *testing.T) {
	billing := regTestModule{
		name: "billing",
		register: func(reg *Registry) error {
			reg.Routes.Mount("/api/v1/billing", regTestHandler{id: "billing"})
			if err := reg.Permissions.Add("billing:read"); err != nil {
				return err
			}
			return reg.Features.Add(FeatureFlag{Key: "billing.dunning"})
		},
	}
	org := regTestModule{
		name: "org",
		deps: []string{"billing"},
		register: func(reg *Registry) error {
			reg.Routes.Mount("/api/v1/org", regTestHandler{id: "org"})
			if err := reg.Permissions.Add("org:read"); err != nil {
				return err
			}
			// Depends on a flag the module registered before it declared.
			return reg.Features.Add(FeatureFlag{
				Key:       "org.workspaces",
				DependsOn: []string{"billing.dunning"},
			})
		},
	}

	kernel := NewKernel(DeploymentModeStandalone)
	reg, err := kernel.Bootstrap(context.Background(), org, billing)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}

	if got := len(reg.Routes.Routes()); got != 2 {
		t.Errorf("Routes() returned %d routes, want both modules' routes", got)
	}
	if got := len(reg.Permissions.Permissions()); got != 2 {
		t.Errorf("Permissions() returned %d permissions, want both modules' permissions", got)
	}
	if got := len(reg.Features.Flags()); got != 2 {
		t.Errorf("Flags() returned %d flags, want both modules' flags", got)
	}
}

func TestBootstrap_CancelledContext_StopsBeforeRegistering(t *testing.T) {
	var order []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// KVStore, Mailer and ObjectStore are wired too, with the standalone
	// defaults: the distributed mode requires all four seams, and this test's
	// subject is the context-cancellation check, not wiring.
	kernel := NewKernel(DeploymentModeDistributed,
		WithEventBus(newRegTestBus()), WithKVStore(NewMemoryKVStore()),
		WithMailer(NewConsoleMailer()), WithObjectStore(NewLocalObjectStore(t.TempDir())))
	reg, err := kernel.Bootstrap(ctx, regTestRecorder("billing", nil, &order))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap context.Canceled", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none", order)
	}
}

func TestKernel_DeploymentMode_ReportsTheConfiguredDeploymentMode(t *testing.T) {
	for _, want := range []DeploymentMode{DeploymentModeStandalone, DeploymentModeDistributed} {
		if got := NewKernel(want).DeploymentMode(); got != want {
			t.Errorf("DeploymentMode() = %q, want %q", got, want)
		}
	}
}

// TestBootstrap_DistributedModeWithoutEventBus_FailsFast pins the fail-fast
// rule from docs/internal/03-deployment-modes.md: the standalone in-memory
// bus is single-process, so a distributed-mode kernel that has none wired in
// must refuse to assemble instead of handing every module a bus its replicas
// cannot share.
func TestBootstrap_DistributedModeWithoutEventBus_FailsFast(t *testing.T) {
	var order []string

	reg, err := NewKernel(DeploymentModeDistributed).Bootstrap(context.Background(), regTestRecorder("billing", nil, &order))

	if !errors.Is(err, ErrMissingDistributedEventBus) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrMissingDistributedEventBus", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none before the wiring was validated", order)
	}
}

// TestBootstrap_DistributedModeWithoutKVStore_FailsFast mirrors
// TestBootstrap_DistributedModeWithoutEventBus_FailsFast for the KVStore
// seam: the standalone in-memory store is single-process, so a
// distributed-mode kernel with none wired in must refuse to assemble instead
// of handing every module a store its replicas cannot share. The bus is
// wired here so the bus check inside Bootstrap, which runs first, passes and
// the failure actually exercises the KVStore check instead of masking it.
func TestBootstrap_DistributedModeWithoutKVStore_FailsFast(t *testing.T) {
	var order []string

	kernel := NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithMailer(NewConsoleMailer()))
	reg, err := kernel.Bootstrap(context.Background(), regTestRecorder("billing", nil, &order))

	if !errors.Is(err, ErrMissingDistributedKVStore) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrMissingDistributedKVStore", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none before the wiring was validated", order)
	}
}

func TestBootstrap_WiresTheDeploymentModeEventBusIntoTheRegistry(t *testing.T) {
	tests := []struct {
		name string
		// kernel receives the injectable stand-in for a distributed bus, and
		// reports whether the assembled registry must end up wired to it.
		kernel       func(injected EventBus) *Kernel
		wantInjected bool
	}{
		{
			name:   "the standalone deployment mode falls back to the in-memory bus",
			kernel: func(EventBus) *Kernel { return NewKernel(DeploymentModeStandalone) },
		},
		{
			name:         "an injected bus replaces the standalone default",
			kernel:       func(injected EventBus) *Kernel { return NewKernel(DeploymentModeStandalone, WithEventBus(injected)) },
			wantInjected: true,
		},
		{
			name: "the distributed deployment mode uses the injected bus",
			kernel: func(injected EventBus) *Kernel {
				// KVStore, Mailer and ObjectStore are wired too, with the
				// standalone defaults: this table exercises the EventBus seam
				// specifically, and the distributed mode also requires the
				// other seams, so leaving them unwired would fail Bootstrap
				// before the EventBus wiring under test even runs.
				return NewKernel(DeploymentModeDistributed, WithEventBus(injected), WithKVStore(NewMemoryKVStore()), WithMailer(NewConsoleMailer()), WithObjectStore(NewLocalObjectStore(t.TempDir())))
			},
			wantInjected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injected := newRegTestBus()
			subscriber := regTestModule{
				name: "billing",
				register: func(reg *Registry) error {
					reg.Events.Subscribe("org.member.invited", func(context.Context, Event) error { return nil })
					return nil
				},
			}

			reg, err := tt.kernel(injected).Bootstrap(context.Background(), subscriber)
			if err != nil {
				t.Fatalf("Bootstrap() error = %v, want nil", err)
			}
			if reg.EventBus() == nil {
				t.Fatal("EventBus() is nil")
			}
			if tt.wantInjected {
				if reg.EventBus() != EventBus(injected) {
					t.Fatalf("EventBus() = %v, want the bus wired into the kernel", reg.EventBus())
				}
				// The module subscribed through reg.Events, so the bus the
				// host publishes into is the one that got the subscription.
				if got := injected.subscriptions(); len(got) != 1 || got[0] != "org.member.invited" {
					t.Errorf("wired bus recorded subscriptions %v, want [org.member.invited]", got)
				}
				return
			}
			if reg.EventBus() == EventBus(injected) {
				t.Error("EventBus() returned the bus that was never wired in")
			}
			if got := len(injected.subscriptions()); got != 0 {
				t.Errorf("uninjected bus recorded %d subscriptions, want 0", got)
			}
		})
	}
}

// TestBootstrap_WiresTheDeploymentModeKVStoreIntoTheRegistry mirrors
// TestBootstrap_WiresTheDeploymentModeEventBusIntoTheRegistry for the KVStore
// seam: the same three scenarios (standalone default, an injected override on
// the standalone deployment mode, and the distributed mode, which has no
// default of its own). The bus, Mailer and ObjectStore are wired
// unconditionally in the distributed case here, for the same reason the bus
// table wires KVStore in its own distributed case: this table exercises
// KVStore specifically, and the distributed mode requires all four seams.
func TestBootstrap_WiresTheDeploymentModeKVStoreIntoTheRegistry(t *testing.T) {
	tests := []struct {
		name string
		// kernel receives the injectable stand-in for a distributed store,
		// and reports whether the assembled registry must end up wired to it.
		kernel       func(injected KVStore) *Kernel
		wantInjected bool
	}{
		{
			name:   "the standalone deployment mode falls back to the in-memory store",
			kernel: func(KVStore) *Kernel { return NewKernel(DeploymentModeStandalone) },
		},
		{
			name:         "an injected store replaces the standalone default",
			kernel:       func(injected KVStore) *Kernel { return NewKernel(DeploymentModeStandalone, WithKVStore(injected)) },
			wantInjected: true,
		},
		{
			name: "the distributed deployment mode uses the injected store",
			kernel: func(injected KVStore) *Kernel {
				return NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithKVStore(injected), WithMailer(NewConsoleMailer()), WithObjectStore(NewLocalObjectStore(t.TempDir())))
			},
			wantInjected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injected := newRegTestKVStore()
			writer := regTestModule{
				name: "billing",
				register: func(reg *Registry) error {
					return reg.KVStore().Set(context.Background(), "billing:seeded", []byte("1"), 0)
				},
			}

			reg, err := tt.kernel(injected).Bootstrap(context.Background(), writer)
			if err != nil {
				t.Fatalf("Bootstrap() error = %v, want nil", err)
			}
			if reg.KVStore() == nil {
				t.Fatal("KVStore() is nil")
			}
			if tt.wantInjected {
				if reg.KVStore() != KVStore(injected) {
					t.Fatalf("KVStore() = %v, want the store wired into the kernel", reg.KVStore())
				}
				// The module wrote through reg.KVStore(), so the store the
				// host wired in is the one that actually received the write.
				if got := injected.setKeys(); len(got) != 1 || got[0] != "billing:seeded" {
					t.Errorf("wired store recorded sets %v, want [billing:seeded]", got)
				}
				return
			}
			if reg.KVStore() == KVStore(injected) {
				t.Error("KVStore() returned the store that was never wired in")
			}
			if got := len(injected.setKeys()); got != 0 {
				t.Errorf("uninjected store recorded %d sets, want 0", got)
			}
		})
	}
}

func TestBootstrap_UnknownDeploymentMode_ReturnsError(t *testing.T) {
	var order []string

	reg, err := NewKernel(DeploymentMode("staging")).Bootstrap(context.Background(), regTestRecorder("billing", nil, &order))

	if !errors.Is(err, ErrInvalidDeploymentMode) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrInvalidDeploymentMode", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error = %q, want it to name the unknown deployment mode", err)
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none", order)
	}
}

func TestWithEventBus_NilBusKeepsTheDeploymentModeDefault(t *testing.T) {
	reg, err := NewKernel(DeploymentModeStandalone, WithEventBus(nil)).Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if reg.EventBus() == nil {
		t.Error("EventBus() is nil, want the standalone deployment mode's default")
	}
}

// TestWithKVStore_NilStoreKeepsTheDeploymentModeDefault mirrors
// TestWithEventBus_NilBusKeepsTheDeploymentModeDefault for the key-value seam.
func TestWithKVStore_NilStoreKeepsTheDeploymentModeDefault(t *testing.T) {
	reg, err := NewKernel(DeploymentModeStandalone, WithKVStore(nil)).Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if reg.KVStore() == nil {
		t.Error("KVStore() is nil, want the standalone deployment mode's default")
	}
}

// TestBootstrap_DistributedModeWithoutMailer_FailsFast mirrors
// TestBootstrap_DistributedModeWithoutEventBus_FailsFast and its KVStore
// counterpart for the mail seam: the standalone console mailer prints to a
// process's stdout, so a distributed-mode kernel with none wired in must
// refuse to assemble instead of handing every module a mailer whose output
// nobody reads. Bus and KVStore are wired here so their checks inside
// Bootstrap, which run first, pass and the failure actually exercises the
// Mailer check instead of masking it.
func TestBootstrap_DistributedModeWithoutMailer_FailsFast(t *testing.T) {
	var order []string

	kernel := NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithKVStore(NewMemoryKVStore()))
	reg, err := kernel.Bootstrap(context.Background(), regTestRecorder("billing", nil, &order))

	if !errors.Is(err, ErrMissingDistributedMailer) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrMissingDistributedMailer", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none before the wiring was validated", order)
	}
}

// TestBootstrap_DistributedModeWithoutObjectStore_FailsFast mirrors
// TestBootstrap_DistributedModeWithoutMailer_FailsFast and its older
// counterparts for the object-store seam: the standalone deployment mode's
// local store is a throwaway directory on one host's disk, so a
// distributed-mode kernel with none wired in must refuse to assemble instead
// of handing every module a store whose objects its replicas can never see.
// Bus, KVStore and Mailer are wired here so their checks inside Bootstrap,
// which run first, pass and the failure actually exercises the ObjectStore
// check instead of masking it.
func TestBootstrap_DistributedModeWithoutObjectStore_FailsFast(t *testing.T) {
	var order []string

	kernel := NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithKVStore(NewMemoryKVStore()), WithMailer(NewConsoleMailer()))
	reg, err := kernel.Bootstrap(context.Background(), regTestRecorder("billing", nil, &order))

	if !errors.Is(err, ErrMissingDistributedObjectStore) {
		t.Fatalf("Bootstrap() error = %v, want it to wrap ErrMissingDistributedObjectStore", err)
	}
	if reg != nil {
		t.Error("Bootstrap() returned a registry alongside the error, want nil")
	}
	if len(order) != 0 {
		t.Errorf("modules registered = %v, want none before the wiring was validated", order)
	}
}

// TestBootstrap_WiresTheDeploymentModeMailerIntoTheRegistry mirrors
// TestBootstrap_WiresTheDeploymentModeEventBusIntoTheRegistry and its KVStore
// counterpart for the mail seam: the same three scenarios (standalone
// default, an injected override on the standalone deployment mode, and the
// distributed mode, which has no default of its own). Bus and KVStore are
// wired unconditionally in the distributed case here, for the same reason the
// other tables wire their seams: this table exercises Mailer specifically,
// and the distributed mode requires all of them.
func TestBootstrap_WiresTheDeploymentModeMailerIntoTheRegistry(t *testing.T) {
	tests := []struct {
		name string
		// kernel receives the injectable stand-in for a distributed mailer,
		// and reports whether the assembled registry must end up wired to it.
		kernel       func(injected Mailer) *Kernel
		wantInjected bool
	}{
		{
			name:   "the standalone deployment mode falls back to the console mailer",
			kernel: func(Mailer) *Kernel { return NewKernel(DeploymentModeStandalone) },
		},
		{
			name:         "an injected mailer replaces the standalone default",
			kernel:       func(injected Mailer) *Kernel { return NewKernel(DeploymentModeStandalone, WithMailer(injected)) },
			wantInjected: true,
		},
		{
			name: "the distributed deployment mode uses the injected mailer",
			kernel: func(injected Mailer) *Kernel {
				return NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithKVStore(NewMemoryKVStore()), WithMailer(injected), WithObjectStore(NewLocalObjectStore(t.TempDir())))
			},
			wantInjected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			injected := newConsoleMailer(&out)

			reg, err := tt.kernel(injected).Bootstrap(context.Background())
			if err != nil {
				t.Fatalf("Bootstrap() error = %v, want nil", err)
			}
			if reg.Mailer() == nil {
				t.Fatal("Mailer() is nil")
			}
			if !tt.wantInjected {
				if reg.Mailer() == Mailer(injected) {
					t.Error("Mailer() returned the mailer that was never wired in")
				}
				return
			}
			if reg.Mailer() != Mailer(injected) {
				t.Fatalf("Mailer() = %v, want the mailer wired into the kernel", reg.Mailer())
			}
			// The registry hands out the kernel's mailer, so sending through
			// it must reach the writer of the mailer the host wired in.
			err = reg.Mailer().Send(context.Background(), Mail{
				From:    "ops@example.com",
				To:      []string{"ada@example.com"},
				Subject: "wiring check",
				Text:    "the injected mailer was reached",
			})
			if err != nil {
				t.Fatalf("Send() error = %v, want nil", err)
			}
			if got := out.String(); !strings.Contains(got, "[mail] from: ops@example.com") {
				t.Errorf("wired mailer output = %q, want it to carry the sent message", got)
			}
		})
	}
}

// TestWithMailer_NilMailerKeepsTheDeploymentModeDefault mirrors
// TestWithEventBus_NilBusKeepsTheDeploymentModeDefault and its KVStore
// counterpart for the mail seam.
func TestWithMailer_NilMailerKeepsTheDeploymentModeDefault(t *testing.T) {
	reg, err := NewKernel(DeploymentModeStandalone, WithMailer(nil)).Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if reg.Mailer() == nil {
		t.Error("Mailer() is nil, want the standalone deployment mode's default")
	}
}

// TestWithObjectStore_NilStoreKeepsTheDeploymentModeDefault mirrors
// TestWithMailer_NilMailerKeepsTheDeploymentModeDefault and its older
// counterparts for the object-store seam.
func TestWithObjectStore_NilStoreKeepsTheDeploymentModeDefault(t *testing.T) {
	reg, err := NewKernel(DeploymentModeStandalone, WithObjectStore(nil)).Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}
	if reg.ObjectStore() == nil {
		t.Error("ObjectStore() is nil, want the standalone deployment mode's default")
	}
}

// TestBootstrap_WiresTheDeploymentModeObjectStoreIntoTheRegistry mirrors
// TestBootstrap_WiresTheDeploymentModeMailerIntoTheRegistry and its older
// counterparts for the object-store seam: the same three scenarios
// (standalone default, an injected override on the standalone deployment
// mode, and the distributed mode, which has no default of its own). The
// standalone default is a throwaway directory the kernel creates for itself,
// so it cannot be recognised by identity across bootstraps the way the
// in-memory bus and KVStore can; the table asserts behaviour instead, by
// storing an object through the assembled registry and asking each store
// whether it received it.
func TestBootstrap_WiresTheDeploymentModeObjectStoreIntoTheRegistry(t *testing.T) {
	tests := []struct {
		name string
		// kernel receives the injectable stand-in for a distributed store,
		// and reports whether the assembled registry must end up wired to it.
		kernel       func(injected ObjectStore) *Kernel
		wantInjected bool
	}{
		{
			name:   "the standalone deployment mode falls back to a private store",
			kernel: func(ObjectStore) *Kernel { return NewKernel(DeploymentModeStandalone) },
		},
		{
			name: "an injected store replaces the standalone default",
			kernel: func(injected ObjectStore) *Kernel {
				return NewKernel(DeploymentModeStandalone, WithObjectStore(injected))
			},
			wantInjected: true,
		},
		{
			name: "the distributed deployment mode uses the injected store",
			kernel: func(injected ObjectStore) *Kernel {
				return NewKernel(DeploymentModeDistributed, WithEventBus(NewMemoryEventBus()), WithKVStore(NewMemoryKVStore()), WithMailer(NewConsoleMailer()), WithObjectStore(injected))
			},
			wantInjected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injected := NewLocalObjectStore(t.TempDir())

			// bootstrapErr: the many inner if-init errs below must not shadow
			// an outer one, so the outer name steps aside.
			reg, bootstrapErr := tt.kernel(injected).Bootstrap(context.Background())
			if bootstrapErr != nil {
				t.Fatalf("Bootstrap() error = %v, want nil", bootstrapErr)
			}
			if reg.ObjectStore() == nil {
				t.Fatal("ObjectStore() is nil")
			}
			// The registry hands out the kernel's store, so storing through it
			// must reach the store the host wired in and no other.
			if err := reg.ObjectStore().PutObject(context.Background(), "wiring/check", strings.NewReader("reached")); err != nil {
				t.Fatalf("PutObject() error = %v, want nil", err)
			}
			if tt.wantInjected {
				if reg.ObjectStore() != ObjectStore(injected) {
					t.Fatalf("ObjectStore() = %v, want the store wired into the kernel", reg.ObjectStore())
				}
				reader, err := injected.GetObject(context.Background(), "wiring/check")
				if err != nil {
					t.Errorf("wired store GetObject() error = %v, want the object the module stored", err)
					return
				}
				body, err := io.ReadAll(reader)
				if closeErr := reader.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Errorf("reading the wired store back failed: %v", err)
					return
				}
				if got := string(body); got != "reached" {
					t.Errorf("wired store holds %q, want %q", got, "reached")
				}
				return
			}
			if reg.ObjectStore() == ObjectStore(injected) {
				t.Error("ObjectStore() returned the store that was never wired in")
			}
			reader, err := injected.GetObject(context.Background(), "wiring/check")
			if err == nil {
				reader.Close()
				t.Error("uninjected store holds the fallback object, want ErrObjectNotFound: the fallback store must be private to the kernel")
			} else if !errors.Is(err, ErrObjectNotFound) {
				t.Errorf("uninjected store GetObject() error = %v, want ErrObjectNotFound", err)
			}
		})
	}
}

func TestRegistry_ConcurrentRegistration_IsRaceFree(t *testing.T) {
	const goroutines = 8

	reg := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			reg.Routes.Mount("/api/v1/"+id, regTestHandler{id: id})
			if err := reg.Permissions.Add(id + ":read"); err != nil {
				t.Errorf("Permissions.Add() error = %v, want nil", err)
			}
			if err := reg.AuditActions.Add(id + ".created"); err != nil {
				t.Errorf("AuditActions.Add() error = %v, want nil", err)
			}
			if err := reg.Jobs.Handle(id+".job", regTestHandler{id: id}); err != nil {
				t.Errorf("Jobs.Handle() error = %v, want nil", err)
			}
			if err := reg.Config.Add(ConfigItem{Key: id + ".key", Type: "string"}); err != nil {
				t.Errorf("Config.Add() error = %v, want nil", err)
			}
			if err := reg.Features.Add(FeatureFlag{Key: id + ".flag"}); err != nil {
				t.Errorf("Features.Add() error = %v, want nil", err)
			}
			if err := reg.Events.Publishes(EventDecl{Type: id + ".created"}); err != nil {
				t.Errorf("Events.Publishes() error = %v, want nil", err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(reg.Routes.Routes()); got != goroutines {
		t.Errorf("Routes() returned %d routes, want %d", got, goroutines)
	}
	if got := len(reg.Permissions.Permissions()); got != goroutines {
		t.Errorf("Permissions() returned %d permissions, want %d", got, goroutines)
	}
	if got := len(reg.AuditActions.Actions()); got != goroutines {
		t.Errorf("Actions() returned %d actions, want %d", got, goroutines)
	}
	if got := len(reg.Jobs.Handlers()); got != goroutines {
		t.Errorf("Handlers() returned %d handlers, want %d", got, goroutines)
	}
	if got := len(reg.Config.Items()); got != goroutines {
		t.Errorf("Items() returned %d items, want %d", got, goroutines)
	}
	if got := len(reg.Features.Flags()); got != goroutines {
		t.Errorf("Flags() returned %d flags, want %d", got, goroutines)
	}
	if got := len(reg.Events.Published()); got != goroutines {
		t.Errorf("Published() returned %d events, want %d", got, goroutines)
	}
	if err := ValidateFeatureGraph(reg); err != nil {
		t.Errorf("ValidateFeatureGraph() error = %v, want nil", err)
	}
}

// localeBundleModule is a Module that ships a real Locales() bundle, so a
// bootstrap test can drive the catalog merge against actual files instead of
// the empty embed.FS every other test double returns.
type localeBundleModule struct {
	name string
}

func (m localeBundleModule) Name() string         { return m.name }
func (m localeBundleModule) DependsOn() []string  { return nil }
func (m localeBundleModule) Migrations() embed.FS { return embed.FS{} }
func (m localeBundleModule) Locales() embed.FS    { return locales.FS }
func (m localeBundleModule) OpenAPISpec() []byte  { return nil }

func (m localeBundleModule) Register(*Registry) error { return nil }

// localeBundleModule.Locales returns the pkgcore seed bundle through its own
// locales package, so this merge test consumes the same bytes the package
// ships to consumers.
func TestBootstrap_AssemblesCatalogFromModuleLocaleFiles(t *testing.T) {
	reg, err := NewKernel(DeploymentModeStandalone).Bootstrap(context.Background(),
		localeBundleModule{name: "pkgcore"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := reg.Locales()
	if catalog == nil {
		t.Fatal("reg.Locales() = nil after Bootstrap, want the merged catalog")
	}
	text, err := catalog.Lookup(i18n.LocaleENUS, "pkgcore.seed.params",
		map[string]any{"Name": "bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "bootstrap") {
		t.Errorf("en-US seed.params through the bootstrapped registry = %q", text)
	}
	zhText, err := catalog.LookupPlural(i18n.LocaleZHCN, "pkgcore.seed.plural", 3,
		map[string]any{"Count": 3})
	if err != nil {
		t.Fatal(err)
	}
	if zhText == "" || zhText == text {
		t.Errorf("zh-CN seed.plural = %q, want a real zh-CN rendering", zhText)
	}
}

func TestBootstrap_EmptyLocaleFilesYieldEmptyCatalog_HandBuiltRegistryStaysNil(t *testing.T) {
	// Every other test double ships an empty Locales() embed.FS, which
	// contributes no messages: Bootstrap still succeeds, and the registry it
	// produces carries an empty catalog -- one that renders nothing, but is
	// still a catalog, because Bootstrap always installs the frozen merge.
	// A hand-built Registry, by contrast, never has a catalog at all: the
	// seam is installed by Bootstrap alone, exactly like ObjectStore.
	reg, err := NewKernel(DeploymentModeStandalone).Bootstrap(context.Background(),
		regTestModule{name: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := reg.Locales()
	if catalog == nil {
		t.Fatal("reg.Locales() = nil after Bootstrap, want an empty catalog")
	}
	if got := catalog.Locales(); len(got) != 0 {
		t.Errorf("catalog.Locales() = %v after a bootstrap with no locale files, want none", got)
	}
	handBuilt := NewRegistry(NewMemoryEventBus(), NewMemoryKVStore(), NewConsoleMailer())
	if catalog := handBuilt.Locales(); catalog != nil {
		t.Errorf("hand-built Registry.Locales() = %v, want nil", catalog)
	}
}
