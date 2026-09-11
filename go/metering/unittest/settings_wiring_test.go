// This suite lives in package unittest -- this module's dedicated unit-test
// directory for unit-tier checks with no single source file as their target
// (the backend coding standard's testing-layout rule). It exercises
// metering's dynamic-configuration wiring end to end, black-box: a REAL
// config module stores metering.default_overage_threshold, the module's
// SettingsReader seam carries it, and the overage event the declaration
// promises fires (or not) accordingly.
package unittest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/metering/migrations"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// eventProbe records bus events of one type.
type eventProbe struct {
	mu     sync.Mutex
	events []pkgcore.Event
}

func (p *eventProbe) handler(_ context.Context, evt pkgcore.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, evt)
	return nil
}

func (p *eventProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// TestDefaultOverageThresholdConfigItem_ControlsTheCrossingEventEndToEnd is
// the end-to-end proof for metering's declared dynamic items: with the
// settings seam wired, an explicit metering.default_overage_threshold row in
// the real configs table makes the next Ingest that reaches the threshold
// publish EventOverageThresholdCrossed -- without it, nothing links any
// threshold and the event never fires.
func TestDefaultOverageThresholdConfigItem_ControlsTheCrossingEventEndToEnd(t *testing.T) {
	type run struct {
		wired    bool
		rowSet   bool
		rowValue int64
		want     int
	}

	for _, tc := range []struct {
		name string
		run  run
	}{
		{name: "wired reader honors an explicit row (the event fires)", run: run{wired: true, rowSet: true, rowValue: 5, want: 1}},
		{name: "unwired reader ignores the row (the switch was dead)", run: run{wired: false, rowSet: true, rowValue: 5, want: 0}},
		{name: "no row means no threshold and no event", run: run{wired: true, rowSet: false, want: 0}},
		{name: "an explicit zero row turns the default off", run: run{wired: true, rowSet: true, rowValue: 0, want: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := dbtest.NewSQLite(t)
			dbtest.Migrate(t, db, dbkit.DialectSQLite,
				dbtest.Migration{Module: "metering", FS: migrations.FS},
				dbtest.Migration{Module: "config", FS: configmigrations.FS},
			)

			bus := pkgcore.NewMemoryEventBus()
			probe := &eventProbe{}
			bus.Subscribe(metering.EventOverageThresholdCrossed, probe.handler)

			cipher, err := dbkit.NewCipher([]byte("metering-settings-wiring-testkey"))
			if err != nil {
				t.Fatalf("dbkit.NewCipher: %v", err)
			}
			cfgModule := config.NewModule(db, config.WithCipher(cipher), config.WithPollInterval(0))

			opts := []metering.Option{}
			if tc.run.wired {
				opts = append(opts, metering.WithSettingsReader(cfgModule.Handle()))
			}
			m := metering.NewModule(db, opts...)

			reg := componenttest.NewRegistryWithBus(bus)
			if declareErr := componenttest.DeclareInto(reg, m, cfgModule); declareErr != nil {
				t.Fatalf("declare both modules: %v", declareErr)
			}
			cfgSvc, attachErr := cfgModule.Attach(reg)
			if attachErr != nil {
				t.Fatalf("config module Attach: %v", attachErr)
			}

			if tc.run.rowSet {
				pkgcore.RegisterSystemPurpose(config.SystemPurposeSystemWrite)
				sysCtx, sysErr := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
					Actor:   "ops-1",
					Purpose: config.SystemPurposeSystemWrite,
					Ticket:  "ticket-42",
				})
				if sysErr != nil {
					t.Fatalf("pkgcore.WithSystemContext: %v", sysErr)
				}
				if setErr := cfgSvc.Set(sysCtx, config.ScopeSystem, metering.ConfigDefaultOverageThreshold,
					config.Value{Data: tc.run.rowValue}, "ops-1"); setErr != nil {
					t.Fatalf("Set %s: %v", metering.ConfigDefaultOverageThreshold, setErr)
				}
			}

			if err := m.Aggregator().Ingest(context.Background(), metering.UsageEvent{
				TenantID:       "tenant-a",
				Feature:        "ai.generation",
				Quantity:       6,
				IdempotencyKey: "evt-1",
				OccurredAt:     time.Now(),
			}); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if got := probe.count(); got != tc.run.want {
				t.Fatalf("EventOverageThresholdCrossed count = %d; want %d", got, tc.run.want)
			}
		})
	}
}
