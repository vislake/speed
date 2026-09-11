package storage

import (
	"context"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/storage/internal/testutil"
	"github.com/vislake/speed/go/storage/migrations"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the ConfigSchema's empty-config
// decode and the shape of every Requires/Provides token.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, storageComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the two products it consumes at construction, with a
// configuration block covering both the scalar knobs and the boolean one,
// and pins that every value reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(testutil.NewSQLite(t, moduleName, migrations.FS))
	reg.Put(stubQueue{})

	instance, err := storageComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(map[string]any{
		"max_upload_bytes":  int64(4096),
		"upload_ttl":        "15m",
		"no_expiry_allowed": true,
		"allowed_types":     []any{"image/jpeg"},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *storage.Module", instance, instance)
	}
	if m.maxUploadBytes != 4096 {
		t.Errorf("maxUploadBytes = %d, want the configured 4096", m.maxUploadBytes)
	}
	if m.uploadTTL != 15*time.Minute {
		t.Errorf("uploadTTL = %v, want the configured 15m", m.uploadTTL)
	}
	if !m.noExpiryAllowed {
		t.Error("noExpiryAllowed = false, want the configured true")
	}
	if len(m.allowedTypes) != 1 || m.allowedTypes[0] != "image/jpeg" {
		t.Errorf("allowedTypes = %v, want the configured [image/jpeg]", m.allowedTypes)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape: a
// registry carrying none of the component's required products fails the
// construction naming the missing one instead of building a half-wired
// module.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := storageComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}
