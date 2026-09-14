package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// scenarioEnv names the scenario to run. It is read from the environment rather
// than from a positional argument because the configuration loader rejects
// positional arguments, and it stays outside the host's own prefix so that it is
// not reported as an input item nobody reads.
const scenarioEnv = "SPEED_DB_E2E_CASE"

// The scenarios. Each one is a whole run of the host: one process, one registry,
// one assembly.
const (
	// caseFirstMigration is one replica's first start against an empty
	// database, which two of them make at once under a rendezvous.
	caseFirstMigration = "first-migration"
	// caseSerializerRoundTrip writes and reads one encrypted field, in a
	// process that has done nothing else with the database.
	caseSerializerRoundTrip = "serializer-round-trip"
	// caseOneEngineConfigured starts up with one of the two imported
	// implementations configured.
	caseOneEngineConfigured = "one-engine-configured"
	// caseBothEnginesConfigured configures both of them, which is not an
	// assembly this release unit can serve.
	caseBothEnginesConfigured = "both-engines-configured"
	// caseNoEngineConfigured gives neither of them a locator, while a
	// dependant requires the capability.
	caseNoEngineConfigured = "no-engine-configured"
)

// The variables that drive the concurrency scenario, and the paths they carry.
// Like the scenario selector they stay outside the host's prefix, for the same
// reason.
const (
	// roleEnv is what this replica goes by. It is also the name its locator
	// gives the sessions it opens, which is what the applied-noop division is
	// read off afterwards.
	roleEnv = "SPEED_DB_E2E_ROLE"
	// rendezvousEnv is the directory the replicas meet in: one file per
	// replica, written on arrival and watched until every replica is there.
	rendezvousEnv = "SPEED_DB_E2E_RENDEZVOUS"
)

// probeModuleName is the dependant's name in the registry. Nothing else takes
// part in the migration record table under it, so a row keyed by it is this
// run's own work.
const probeModuleName = "probe"

// The markers the concurrency case reads. Each carries the moment it was
// reached, so that a run in which the replicas overlapped can be told from one
// in which the second arrived after the first had finished — which is the
// difference between a run that says something about the mutex and one that says
// nothing.
const (
	// markStart is written when this replica has all but reached its
	// migration stage.
	markStart = "start"
	// markApplied is written by the replica whose own session executed the
	// migration.
	markApplied = "applied"
	// markNoop is written by the replica that found the work already done.
	markNoop = "noop"
)

// roundTripMarker is written by the serializer scenario once the plaintext it
// wrote has come back out of the encrypted column. It is written only after the
// comparison, so the case cannot pass on a run that reached the write and
// nothing else.
const roundTripMarker = "dbhost: round-trip ok"

// appliersTable is where the migration records the application_name of the
// session that executed it.
const appliersTable = "dbhost_appliers"

// replicas is how many processes the concurrency scenario starts, and
// rendezvousTimeout bounds the wait for the last of them, so that a replica
// which never starts fails the run rather than hanging it.
const (
	replicas          = 2
	rendezvousTimeout = 60 * time.Second
)

// migrationFiles carries the migration set this host declares. The embedded
// directory holds one subdirectory per dialect, which is the shape the Migrations
// resource asks for: the file below sits under postgres/, named by the dialect
// rather than by any constant this file could read, so a run configured on the
// other engine finds no subdirectory for it and declares no migrations at all.
//
//go:embed migrations
var migrationFiles embed.FS

// declaredMigrations is the declaration this host hands the database module.
//
// The declaration is unconditional and the dialect decides what it contributes:
// the concurrency scenario runs on PostgreSQL and reads the file above, while a
// scenario configured on another engine finds no subdirectory for it and
// declares zero migrations for that engine — which is how a module states that
// it does not support an engine, and is not a failure.
//
// The subdirectory is reached rather than spelled into the resource because the
// module reads the set relative to its own root: an FS whose top level is this
// package's own directory would put every dialect one level too deep, and the
// run would find no migrations at all and say nothing about it.
func declaredMigrations() db.Migrations {
	set, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// The tree is embedded in this package, so this is a defect in the
		// file that names it rather than anything a run can meet. It is
		// raised where it is written, the treatment this module gives a
		// defective Spec, because a host would have nothing to handle.
		panic("dbhost: the embedded migrations are not where this file puts them: " + err.Error())
	}
	return db.Migrations{FS: set}
}

// probe is the dependant's product, carrying what a scenario has to hand from
// one stage to the next: the capability, and the name this replica's sessions go
// by.
type probe struct {
	capability db.Database
	identity   string
}

// probeNew takes up the capability and runs the part of a scenario that falls in
// the construction stage.
func probeNew(_ context.Context, reg *core.Registry) (any, error) {
	capability, err := core.Resolve[db.Database](reg)
	if err != nil {
		return nil, err
	}
	p := &probe{capability: capability}
	switch scenario() {
	case caseFirstMigration:
		if err := p.joinReplicas(); err != nil {
			return nil, err
		}
	case caseSerializerRoundTrip:
		if err := p.encryptedRoundTrip(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// probeMigrate reports what this replica did with the migration, which can only
// be said once the migration has been applied — and a dependant's Migrate runs
// after the one that applies them, by the dependency it declared.
func probeMigrate(_ context.Context, _ *core.Registry, instance any) error {
	p, ok := instance.(*probe)
	if !ok || p == nil {
		return nil
	}
	if scenario() != caseFirstMigration {
		return nil
	}
	return p.reportWhoApplied()
}

// joinReplicas meets the other replica at the rendezvous and marks the moment
// this process has all but reached its migration stage.
//
// The marker is written here, in the construction stage, because that is the
// last point of a run this host controls: the migration is the database
// module's own callback, and a dependant's Migrate runs after it, once the mutex
// has been given up. Every construction callback of every module runs before the
// first Migrate callback, so a marker written here precedes this process's
// migration — which is what the overlap assertion reads.
func (p *probe) joinReplicas() error {
	identity, err := p.sessionIdentity()
	if err != nil {
		return err
	}
	p.identity = identity
	role := os.Getenv(roleEnv)
	switch {
	case role == "":
		return fmt.Errorf("%s is not set, and this scenario needs the name this replica goes by", roleEnv)
	case identity == "":
		return fmt.Errorf("the locator gives this session no application_name, so the migration cannot " +
			"record which replica ran it and the two would be indistinguishable afterwards; give every " +
			"replica a locator that names it")
	case identity != role:
		return fmt.Errorf("%s calls this replica %q and the locator names its sessions %q: the migration "+
			"records the session that ran it, so the two have to agree", roleEnv, role, identity)
	}
	if err := meetOtherReplicas(role); err != nil {
		return err
	}
	mark(markStart)
	return nil
}

// sessionIdentity reads the name this replica's connections go by.
//
// It is the session's own answer rather than a value this host remembers,
// because that is what the migration records: both are set from the same locator
// at connection time, and reading one back is what keeps the two from drifting.
func (p *probe) sessionIdentity() (string, error) {
	var identity string
	if err := p.capability.DB().Raw(`SELECT current_setting('application_name')`).Scan(&identity).Error; err != nil {
		return "", fmt.Errorf("reading this session's application_name: %w", err)
	}
	return identity, nil
}

// reportWhoApplied prints the marker the concurrency case divides the replicas
// by: applied when this replica's own session executed the migration, noop when
// it found the work already done.
//
// The row the migration wrote is the whole of the evidence, and it has to be: a
// replica that stood down sees the same widgets table, the same single row in
// the record table and a timestamp no later than its own migration stage began.
// Anything else either replica could observe after the fact is identical on both
// sides, so a run with no mutex at all would produce the same picture as one that
// serialised its replicas — except that both of them would have written this
// row.
func (p *probe) reportWhoApplied() error {
	var appliers []string
	if err := p.capability.DB().Raw(`SELECT role FROM ` + appliersTable).Scan(&appliers).Error; err != nil {
		return fmt.Errorf("reading which session ran the migration: %w", err)
	}
	switch {
	case len(appliers) == 0:
		return fmt.Errorf("the migration left no record of the session that ran it, so this run cannot " +
			"say which replica applied it")
	case len(appliers) > 1:
		return fmt.Errorf("%d sessions ran the migration: %s", len(appliers), strings.Join(appliers, ", "))
	case appliers[0] == p.identity:
		mark(markApplied)
	default:
		mark(markNoop)
	}
	return nil
}

// roundTripPlaintext is what the serializer scenario writes into an encrypted
// field and reads back.
const roundTripPlaintext = "dbhost-e2e-plaintext"

// secret is the model the round-trip travels through. Its only interesting
// property is the serializer tag: GORM resolves the name against its
// process-wide table the first time a model carrying it is parsed, and parsing
// this model is the first thing in this process that needs the serializer to be
// there.
type secret struct {
	ID    uint
	Value string `gorm:"serializer:encrypted"`
}

// encryptedRoundTrip writes one model through the delivered handle and reads it
// back.
//
// This process is the whole of the observation. The serializer lives in GORM's
// process-wide table, so a test binary that had already run any other case would
// find it there whatever the assembly did; in a process that has just started,
// the table is empty until the database module puts the serializer in it. An
// implementation that registered it after delivering the handle therefore fails
// here, with the serializer unresolved, on the first statement that carries a
// field marked with it.
func (p *probe) encryptedRoundTrip() error {
	handle := p.capability.DB()
	if err := handle.AutoMigrate(&secret{}); err != nil {
		return fmt.Errorf("creating the table the encrypted field lives in: %w", err)
	}
	if err := handle.Create(&secret{Value: roundTripPlaintext}).Error; err != nil {
		return fmt.Errorf("writing the encrypted field: %w", err)
	}
	var read secret
	if err := handle.First(&read, 1).Error; err != nil {
		return fmt.Errorf("reading the encrypted field back: %w", err)
	}
	if read.Value != roundTripPlaintext {
		return fmt.Errorf("the encrypted field came back as %q, and this run wrote %q",
			read.Value, roundTripPlaintext)
	}
	say(roundTripMarker)
	return nil
}

// meetOtherReplicas blocks until every replica has reached this point, so that
// they go for the migration mutex together rather than one after the other.
func meetOtherReplicas(role string) error {
	dir := os.Getenv(rendezvousEnv)
	if dir == "" {
		return fmt.Errorf("%s is not set, and this scenario needs the directory the replicas meet in",
			rendezvousEnv)
	}
	// The directory is named by the process that started this one, so the file
	// is created through a root that holds it rather than by joining the two
	// pieces into a path: a name that tried to leave the directory — a
	// separator or a parent reference in it — is refused here, and nothing this
	// host is handed can put a file somewhere else.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("opening the rendezvous directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	announcement, err := root.Create("ready-" + role)
	if err != nil {
		return fmt.Errorf("announcing this replica at the rendezvous: %w", err)
	}
	if err := announcement.Close(); err != nil {
		return fmt.Errorf("closing this replica's announcement: %w", err)
	}
	deadline := time.Now().Add(rendezvousTimeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("reading the rendezvous: %w", err)
		}
		arrived := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ready-") {
				arrived++
			}
		}
		if arrived >= replicas {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("only some of the %d replicas reached the rendezvous within %s",
		replicas, rendezvousTimeout)
}

// scenario reports which scenario this run was asked for.
func scenario() string { return os.Getenv(scenarioEnv) }

// mark prints a marker and the moment this process reached it.
//
// The moments are what the overlap assertion reads: without them a run in which
// one replica arrived after the other had finished applying is
// indistinguishable from one in which the two overlapped.
func mark(name string) {
	say(name + "\t" + time.Now().UTC().Format(time.RFC3339Nano))
}
