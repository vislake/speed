package pkgcore

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// regTestHandler is a comparable http.Handler, so a test can assert which
// handler ended up mounted at which path.
type regTestHandler struct{ id string }

func (regTestHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

// seatRegistrars bundles the gate-free in-memory registrars the declaration
// seats are built on, so a registrar's own contract -- ordering, duplicate
// refusal, validation, copy-on-read -- can be exercised without driving an
// assembly through the seats' stage gate. The gate itself is the component
// assembly's (component_registry.go), pinned by its own suites.
type seatRegistrars struct {
	Config        *memoryConfigRegistrar
	Features      *memoryFeatureRegistrar
	Permissions   *memoryPermissionRegistrar
	Jobs          *memoryJobRegistrar
	Notifications *memoryNotificationRegistrar
	Events        *memoryEventRegistrar
	AuditActions  *memoryAuditActionRegistrar
	Retention     *memoryRetentionRegistrar
	Schedules     *memoryScheduleRegistrar
}

func newSeatRegistrars() *seatRegistrars {
	return &seatRegistrars{
		Config:        &memoryConfigRegistrar{keys: make(map[string]struct{})},
		Features:      &memoryFeatureRegistrar{keys: make(map[string]struct{})},
		Permissions:   &memoryPermissionRegistrar{perms: make(map[string]struct{})},
		Jobs:          &memoryJobRegistrar{handlers: make(map[string]any)},
		Notifications: &memoryNotificationRegistrar{keys: make(map[string]struct{})},
		Events:        &memoryEventRegistrar{bus: NewMemoryEventBus(), types: make(map[string]struct{})},
		AuditActions:  &memoryAuditActionRegistrar{actions: make(map[string]struct{})},
		Retention:     &memoryRetentionRegistrar{names: make(map[string]struct{})},
		Schedules:     &memoryScheduleRegistrar{types: make(map[string]struct{})},
	}
}

// EventBus returns the bus the Events registrar installs subscriptions on.
func (r *seatRegistrars) EventBus() EventBus { return r.Events.Bus() }

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
			reg := newSeatRegistrars()
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
	reg := newSeatRegistrars()
	want := ConfigItem{
		Key:         "billing.api_key",
		Type:        "string",
		Default:     "",
		Sensitive:   true,
		Description: "Payment provider API key.",
		Group:       "billing.secrets",
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

func TestRetentionRegistrar_Add_RefusesNilSweep(t *testing.T) {
	sweep := func(context.Context, TenantID, time.Time) (int, error) { return 0, nil }
	erase := func(context.Context, SubjectRef) (int, error) { return 0, nil }
	wellFormed := func(name string) RetentionParticipant {
		return RetentionParticipant{Name: name, Sweep: sweep, Erase: erase}
	}

	tests := []struct {
		name       string
		seed       []RetentionParticipant // registered before the call under test
		call       []RetentionParticipant // the call under test
		wantErr    error                  // sentinel the error must wrap
		offender   string                 // participant Name the error must carry
		wantStored int                    // participants stored after the refused call
	}{
		{
			name:       "nil Sweep is refused",
			seed:       []RetentionParticipant{wellFormed("notes.note")},
			call:       []RetentionParticipant{{Name: "notes.attachment", Erase: erase}},
			wantErr:    ErrNilRetentionSweep,
			offender:   "notes.attachment",
			wantStored: 1,
		},
		{
			name: "one nil Sweep in a batch refuses the whole call",
			call: []RetentionParticipant{
				wellFormed("notes.note"),
				{Name: "notes.attachment", Erase: erase},
			},
			wantErr:    ErrNilRetentionSweep,
			offender:   "notes.attachment",
			wantStored: 0,
		},
		{
			name:       "nil-Sweep validation reports itself before a name collision",
			seed:       []RetentionParticipant{wellFormed("notes.note")},
			call:       []RetentionParticipant{{Name: "notes.note", Erase: erase}},
			wantErr:    ErrNilRetentionSweep,
			offender:   "notes.note",
			wantStored: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			if len(tt.seed) > 0 {
				if err := reg.Retention.Add(tt.seed...); err != nil {
					t.Fatalf("seed Add() error = %v, want nil", err)
				}
			}

			err := reg.Retention.Add(tt.call...)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Add() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.offender) {
				t.Errorf("Add() error = %q, want it to name %q", err, tt.offender)
			}
			// A rejected call must store nothing from that call, leaving
			// only the seed participants registered.
			if got := len(reg.Retention.Participants()); got != tt.wantStored {
				t.Errorf("after the refused Add() there are %d participants, want %d", got, tt.wantStored)
			}
		})
	}
}

func TestRetentionRegistrar_Add_RefusesNilErase(t *testing.T) {
	sweep := func(context.Context, TenantID, time.Time) (int, error) { return 0, nil }
	erase := func(context.Context, SubjectRef) (int, error) { return 0, nil }
	wellFormed := func(name string) RetentionParticipant {
		return RetentionParticipant{Name: name, Sweep: sweep, Erase: erase}
	}

	tests := []struct {
		name       string
		seed       []RetentionParticipant // registered before the call under test
		call       []RetentionParticipant // the call under test
		wantErr    error                  // sentinel the error must wrap
		offender   string                 // participant Name the error must carry
		wantStored int                    // participants stored after the refused call
	}{
		{
			name:       "nil Erase is refused",
			seed:       []RetentionParticipant{wellFormed("notes.note")},
			call:       []RetentionParticipant{{Name: "notes.attachment", Sweep: sweep}},
			wantErr:    ErrNilRetentionErase,
			offender:   "notes.attachment",
			wantStored: 1,
		},
		{
			name: "one nil Erase in a batch refuses the whole call",
			call: []RetentionParticipant{
				wellFormed("notes.note"),
				{Name: "notes.attachment", Sweep: sweep},
			},
			wantErr:    ErrNilRetentionErase,
			offender:   "notes.attachment",
			wantStored: 0,
		},
		{
			name:       "nil-Erase validation reports itself before a name collision",
			seed:       []RetentionParticipant{wellFormed("notes.note")},
			call:       []RetentionParticipant{{Name: "notes.note", Sweep: sweep}},
			wantErr:    ErrNilRetentionErase,
			offender:   "notes.note",
			wantStored: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			if len(tt.seed) > 0 {
				if err := reg.Retention.Add(tt.seed...); err != nil {
					t.Fatalf("seed Add() error = %v, want nil", err)
				}
			}

			err := reg.Retention.Add(tt.call...)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Add() error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.offender) {
				t.Errorf("Add() error = %q, want it to name %q", err, tt.offender)
			}
			// A rejected call must store nothing from that call, leaving
			// only the seed participants registered.
			if got := len(reg.Retention.Participants()); got != tt.wantStored {
				t.Errorf("after the refused Add() there are %d participants, want %d", got, tt.wantStored)
			}
		})
	}
}

func TestRetentionRegistrar_Add_ExportMayStayNil(t *testing.T) {
	noopSweep := func(context.Context, TenantID, time.Time) (int, error) { return 0, nil }
	noopErase := func(context.Context, SubjectRef) (int, error) { return 0, nil }

	tests := []struct {
		name        string
		participant RetentionParticipant
	}{
		{
			name: "Export nil on a participant that did not opt into portability",
			participant: RetentionParticipant{
				Name:  "testutil.notes",
				Sweep: noopSweep,
				Erase: noopErase,
			},
		},
		{
			name: "Export nil on a participant with nothing subject-shaped to erase",
			participant: RetentionParticipant{
				// The sweep-only shape a tenant-wide bundle takes: nothing
				// subject-shaped to erase, no export of its own. The
				// nothing-to-erase fact is stated as an explicit Erase
				// returning (0, nil) -- never as a nil Erase, which the
				// registrar refuses.
				Name:  "compliance.export_manifests",
				Sweep: noopSweep,
				Erase: noopErase,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			if err := reg.Retention.Add(tt.participant); err != nil {
				t.Fatalf("Add() error = %v, want nil for a participant carrying both mandatory callbacks", err)
			}

			got := reg.Retention.Participants()
			if len(got) != 1 {
				t.Fatalf("Participants() returned %d participants, want 1", len(got))
			}
			if got[0].Name != tt.participant.Name {
				t.Errorf("participant Name = %q, want %q", got[0].Name, tt.participant.Name)
			}
			if got[0].Sweep == nil {
				t.Error("registered participant lost its Sweep callback")
			}
			if got[0].Erase == nil {
				t.Error("registered participant lost its Erase callback")
			}
			if got[0].Export != nil {
				t.Error("registered participant gained an Export callback it did not declare")
			}
		})
	}
}

func TestConfigRegistrar_Add_InvalidDeclarationReturnsError(t *testing.T) {
	tests := []struct {
		name    string
		item    ConfigItem
		wantSub string // substring the error message must carry
	}{
		{
			name:    "item without a key",
			item:    ConfigItem{Type: "int", Default: 1},
			wantSub: "without a key",
		},
		{
			name:    "unknown type",
			item:    ConfigItem{Key: "billing.mode", Type: "banana", Default: 1},
			wantSub: `type "banana"`,
		},
		{
			name:    "int default of the wrong Go type",
			item:    ConfigItem{Key: "billing.retry_limit", Type: "int", Default: "3"},
			wantSub: "want int or int64",
		},
		{
			name:    "bool default of the wrong Go type",
			item:    ConfigItem{Key: "billing.dunning", Type: "bool", Default: 1},
			wantSub: "want bool",
		},
		{
			name:    "duration default of the wrong Go type",
			item:    ConfigItem{Key: "jobs.timeout", Type: "duration", Default: 30},
			wantSub: "want time.Duration",
		},
		{
			name:    "int max of the wrong Go type",
			item:    ConfigItem{Key: "billing.retry_limit", Type: "int", Default: 3, Max: "10"},
			wantSub: "want int or int64",
		},
		{
			name:    "string item with Min",
			item:    ConfigItem{Key: "billing.currency", Type: "string", Default: "CNY", Min: "CNY"},
			wantSub: "no ordering",
		},
		{
			name:    "bool item with Max",
			item:    ConfigItem{Key: "billing.dunning", Type: "bool", Default: false, Max: true},
			wantSub: "no ordering",
		},
		{
			name:    "Min above Max",
			item:    ConfigItem{Key: "billing.retry_limit", Type: "int", Min: 10, Max: 1},
			wantSub: "Min 10 above Max 1",
		},
		{
			name:    "duration Min above Max",
			item:    ConfigItem{Key: "jobs.timeout", Type: "duration", Min: time.Minute, Max: time.Second},
			wantSub: "Min 1m0s above Max 1s",
		},
		{
			name:    "default below Min",
			item:    ConfigItem{Key: "billing.retry_limit", Type: "int", Default: 0, Min: 1},
			wantSub: "default 0 falls below its declared Min 1",
		},
		{
			name:    "default above Max",
			item:    ConfigItem{Key: "billing.retry_limit", Type: "int", Default: 3, Max: 2},
			wantSub: "default 3 exceeds its declared Max 2",
		},
		{
			name:    "sensitive default out of range never prints the value",
			item:    ConfigItem{Key: "billing.secret_margin", Type: "int", Sensitive: true, Default: 0, Min: 1},
			wantSub: `key "billing.secret_margin" default falls below its declared Min 1`,
		},
		{
			name:    "Sensitive and Public together",
			item:    ConfigItem{Key: "billing.api_key", Type: "string", Sensitive: true, Public: true},
			wantSub: "both Sensitive and Public",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			err := reg.Config.Add(tt.item)
			if !errors.Is(err, ErrInvalidConfigItem) {
				t.Fatalf("Add() error = %v, want it to wrap ErrInvalidConfigItem", err)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Add() error = %q, want it to contain %q", err, tt.wantSub)
			}
			// A rejected call must register nothing from that call.
			if got := len(reg.Config.Items()); got != 0 {
				t.Errorf("after a rejected Add() there are %d items, want 0", got)
			}
		})
	}
}

func TestConfigRegistrar_Add_RangeDeclarationRoundTrips(t *testing.T) {
	reg := newSeatRegistrars()
	items := []ConfigItem{
		{
			Key:         "billing.retry_limit",
			Type:        "int",
			Default:     3,
			Min:         1,
			Max:         10,
			Group:       "billing",
			Description: "Retries before an invoice run is abandoned.",
		},
		// Bounds and defaults mix int and int64 freely; both live in the
		// same int64 comparison domain.
		{Key: "billing.retry_limit_wide", Type: "int", Default: int64(30), Min: int64(1), Max: int64(100)},
		{Key: "jobs.timeout", Type: "duration", Default: 30 * time.Second, Min: time.Second, Max: time.Hour},
		{Key: "billing.dunning", Type: "bool", Default: false, Public: true, Group: "billing"},
		{Key: "billing.currency", Type: "string", Default: "CNY", Public: true},
		// A nil Default is legal: the module serves no value until one is set.
		{Key: "org.display_name", Type: "string", Public: true},
		{Key: "billing.api_key", Type: "string", Sensitive: true, Group: "billing.secrets"},
		{Key: "org.trial_days", Type: "int", Min: 0},
	}

	if err := reg.Config.Add(items...); err != nil {
		t.Fatalf("Add() error = %v, want nil", err)
	}

	got := reg.Config.Items()
	if len(got) != len(items) {
		t.Fatalf("Items() returned %d items, want %d", len(got), len(items))
	}
	for i := range items {
		if got[i] != items[i] {
			t.Errorf("Items()[%d] = %+v, want %+v", i, got[i], items[i])
		}
	}
}

func TestConfigRegistrar_Add_RejectedCallRegistersNothing(t *testing.T) {
	reg := newSeatRegistrars()
	valid := ConfigItem{Key: "billing.retry_limit", Type: "int", Default: 3}
	invalid := ConfigItem{Key: "billing.retry_limit", Type: "int", Default: "3"}

	err := reg.Config.Add(valid, invalid)
	if !errors.Is(err, ErrInvalidConfigItem) {
		t.Fatalf("Add() error = %v, want it to wrap ErrInvalidConfigItem", err)
	}
	// A rejected call registers nothing, not even the valid sibling.
	if got := len(reg.Config.Items()); got != 0 {
		t.Errorf("after a rejected Add() there are %d items, want 0", got)
	}

	// The valid sibling alone is registrable afterwards, proving the
	// rejection left no partial state behind.
	if err := reg.Config.Add(valid); err != nil {
		t.Fatalf("Add(valid) error = %v, want nil", err)
	}
	if got := len(reg.Config.Items()); got != 1 {
		t.Errorf("Items() returned %d items, want 1", got)
	}
}

func TestPermissionRegistrar_Add_DuplicateAcrossDeclarationsReturnsError(t *testing.T) {
	reg := newSeatRegistrars()

	// Two declaration turns write into the same shared seat, as the assembly
	// drives them.
	if err := reg.Permissions.Add("billing:read", "billing:manage"); err != nil {
		t.Fatalf("first Add() error = %v, want nil", err)
	}

	// A copy-paste mistake: the second declaration claims a billing permission.
	err := reg.Permissions.Add("org:read", "billing:manage")
	if !errors.Is(err, ErrDuplicatePermission) {
		t.Fatalf("second Add() error = %v, want it to wrap ErrDuplicatePermission", err)
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
	reg := newSeatRegistrars()
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
	reg := newSeatRegistrars()
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
	reg := newSeatRegistrars()
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
	reg := newSeatRegistrars()
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
	reg := newSeatRegistrars()

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
			reg := newSeatRegistrars()
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

func TestEventRegistrar_Publishes_RecordsTheDeclaredCatalog(t *testing.T) {
	reg := newSeatRegistrars()

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
			reg := newSeatRegistrars()
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
			reg := newSeatRegistrars()
			if err := reg.Features.Add(tt.flags...); err != nil {
				t.Fatalf("Features.Add() error = %v, want nil", err)
			}

			err := ValidateFeatureGraph(reg.Features)
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
}

func TestRegistry_ConcurrentRegistration_IsRaceFree(t *testing.T) {
	const goroutines = 8

	reg := newSeatRegistrars()
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
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
	if err := ValidateFeatureGraph(reg.Features); err != nil {
		t.Errorf("ValidateFeatureGraph() error = %v, want nil", err)
	}
}

func TestScheduleRegistrar_Add_DeclaresInOrder(t *testing.T) {
	reg := newSeatRegistrars()

	perTenantSweep := PeriodicTask{
		Type:      "storage.expiry_sweep",
		Every:     time.Hour,
		Scope:     PeriodicScopePerTenant,
		KeyPrefix: "storage.sweep:",
	}
	platformScan := PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: TenantID("_pki_platform_scan"),
	}
	if err := reg.Schedules.Add(perTenantSweep, platformScan); err != nil {
		t.Fatalf("Add: %v", err)
	}

	decls := reg.Schedules.Declarations()
	if len(decls) != 2 {
		t.Fatalf("Declarations() returned %d declarations, want 2", len(decls))
	}
	if decls[0] != perTenantSweep || decls[1] != platformScan {
		t.Errorf("Declarations() = %+v, want the two declarations in registration order", decls)
	}

	// A declaration read is a copy: mutating it changes nothing.
	decls[0].Type = "hijacked"
	if got := reg.Schedules.Declarations()[0].Type; got != perTenantSweep.Type {
		t.Errorf("mutating the returned slice changed the registry: type = %q, want %q", got, perTenantSweep.Type)
	}
}

func TestScheduleRegistrar_Add_RejectsDuplicateType(t *testing.T) {
	wellFormed := func(taskType string) PeriodicTask {
		return PeriodicTask{
			Type:      taskType,
			Every:     time.Hour,
			Scope:     PeriodicScopePerTenant,
			KeyPrefix: "sweep:",
		}
	}

	tests := []struct {
		name       string
		seed       []PeriodicTask // registered before the call under test
		call       []PeriodicTask // the call under test
		wantStored int
	}{
		{
			name:       "a type declared twice within one call registers nothing",
			call:       []PeriodicTask{wellFormed("storage.expiry_sweep"), wellFormed("storage.expiry_sweep")},
			wantStored: 0,
		},
		{
			name:       "a type already registered by an earlier call is refused and the seed survives",
			seed:       []PeriodicTask{wellFormed("storage.expiry_sweep")},
			call:       []PeriodicTask{wellFormed("storage.expiry_sweep")},
			wantStored: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			if len(tt.seed) > 0 {
				if err := reg.Schedules.Add(tt.seed...); err != nil {
					t.Fatalf("seeding: Add(%v) = %v, want nil", tt.seed, err)
				}
			}

			err := reg.Schedules.Add(tt.call...)
			if !errors.Is(err, ErrDuplicatePeriodicTask) {
				t.Fatalf("Add(%v) = %v, want an error wrapping ErrDuplicatePeriodicTask", tt.call, err)
			}
			if got := len(reg.Schedules.Declarations()); got != tt.wantStored {
				t.Errorf("stored declarations after the failed call = %d, want %d", got, tt.wantStored)
			}
		})
	}
}

func TestScheduleRegistrar_Add_RejectsContradictoryDeclaration(t *testing.T) {
	wellFormed := func(taskType string) PeriodicTask {
		return PeriodicTask{
			Type:      taskType,
			Every:     time.Hour,
			Scope:     PeriodicScopePerTenant,
			KeyPrefix: "sweep:",
		}
	}

	tests := []struct {
		name string
		seed []PeriodicTask // registered before the call under test
		call []PeriodicTask // the call under test
	}{
		{
			name: "empty task type",
			call: []PeriodicTask{{Type: "", Every: time.Hour, Scope: PeriodicScopePerTenant, KeyPrefix: "sweep:"}},
		},
		{
			name: "zero window",
			call: []PeriodicTask{{Type: "storage.expiry_sweep", Every: 0, Scope: PeriodicScopePerTenant, KeyPrefix: "sweep:"}},
		},
		{
			name: "negative window",
			call: []PeriodicTask{{Type: "storage.expiry_sweep", Every: -time.Minute, Scope: PeriodicScopePerTenant, KeyPrefix: "sweep:"}},
		},
		{
			name: "unknown scope",
			call: []PeriodicTask{{Type: "storage.expiry_sweep", Every: time.Hour, Scope: PeriodicScope("weekly"), KeyPrefix: "sweep:"}},
		},
		{
			name: "empty key prefix",
			call: []PeriodicTask{{Type: "storage.expiry_sweep", Every: time.Hour, Scope: PeriodicScopePerTenant, KeyPrefix: ""}},
		},
		{
			name: "platform scope without a sentinel tenant",
			call: []PeriodicTask{{Type: "pki.expiry_scan", Every: time.Hour, Scope: PeriodicScopePlatform, KeyPrefix: "pki.expiry_scan:"}},
		},
		{
			name: "per-tenant scope carrying a sentinel tenant",
			call: []PeriodicTask{{Type: "storage.expiry_sweep", Every: time.Hour, Scope: PeriodicScopePerTenant, KeyPrefix: "sweep:", PlatformTenant: TenantID("_pki_platform_scan")}},
		},
		{
			name: "a contradictory declaration reports itself before a later item's duplicate",
			seed: []PeriodicTask{wellFormed("storage.expiry_sweep")},
			call: []PeriodicTask{
				wellFormed("storage.expiry_sweep"),
				{Type: "compliance.retention_sweep", Every: 0, Scope: PeriodicScopePerTenant, KeyPrefix: "compliance.retention_sweep:"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newSeatRegistrars()
			if len(tt.seed) > 0 {
				if err := reg.Schedules.Add(tt.seed...); err != nil {
					t.Fatalf("seeding: Add(%v) = %v, want nil", tt.seed, err)
				}
			}

			err := reg.Schedules.Add(tt.call...)
			if !errors.Is(err, ErrInvalidPeriodicTask) {
				t.Fatalf("Add(%v) = %v, want an error wrapping ErrInvalidPeriodicTask", tt.call, err)
			}
			if got := len(reg.Schedules.Declarations()); got != len(tt.seed) {
				t.Errorf("stored declarations after the failed call = %d, want %d (the seed alone)", got, len(tt.seed))
			}
		})
	}
}

func TestRetentionParticipant_DocContractTellsAuthorsWhereErrorTextGoes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "registry.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse registry.go: %v", err)
	}

	var typeDoc, sweepDoc, exportDoc string
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			return true
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "RetentionParticipant" {
				continue
			}
			// A standalone (non-parenthesized) type declaration carries its
			// doc comment on the GenDecl; a grouped one would carry it on
			// the TypeSpec. Read whichever the parser attached it to.
			if gd.Doc != nil {
				typeDoc = gd.Doc.Text()
			} else if ts.Doc != nil {
				typeDoc = ts.Doc.Text()
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					if field.Doc == nil {
						continue
					}
					for _, name := range field.Names {
						switch name.Name {
						case "Sweep":
							sweepDoc = field.Doc.Text()
						case "Export":
							exportDoc = field.Doc.Text()
						}
					}
				}
			}
			return false
		}
		return true
	})

	for _, want := range []string{"error text", "never", "audit", "export manifest"} {
		if !strings.Contains(typeDoc, want) {
			t.Errorf("RetentionParticipant type doc does not state where a callback's error text goes (missing %q)", want)
		}
	}
	for name, doc := range map[string]string{"Sweep": sweepDoc, "Export": exportDoc} {
		if !strings.Contains(doc, "type's doc comment") {
			t.Errorf("RetentionParticipant.%s field doc does not point its author at the type-level error-text contract", name)
		}
	}
	if !strings.Contains(exportDoc, "manifest") {
		t.Errorf("RetentionParticipant.Export field doc does not warn that the error text must never reach the export manifest")
	}
}
