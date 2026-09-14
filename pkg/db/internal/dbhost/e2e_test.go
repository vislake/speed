package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/pkg/db/internal/pgtest"
)

// The cases run the host as a real child process. What they have to prove is
// what a real assembly does: which of two imported implementations the
// configuration selects, what a startup that cannot be served reports, whether a
// dependant finds the serializer in place by the time it is constructed, and
// whether two replicas starting at once against an empty database divide into
// one applier and one no-op. None of that exists inside a test binary, and a
// real assembly cannot be started in one at all — the configuration loader reads
// os.Args[1:], which go test has already filled with arguments of its own.

// hostBinary is the compiled host every case runs. It is built once, in
// TestMain, because building it is by far the slowest thing here.
var hostBinary string

// runLimit bounds one run of the host. It is generous: exceeding it means the
// host hung — a pool of one connection with an extra connection somewhere in the
// run, a mutex that is never acquired — and the case says so with both streams
// attached.
const runLimit = 90 * time.Second

// buildLimit bounds the build of the host binary, which compiles this module's
// dependencies on a cold cache.
const buildLimit = 5 * time.Minute

func TestMain(m *testing.M) {
	// The docker-free case runs this binary again with an environment that has
	// no docker on it, and the child reaches pgtest before it needs a host: a
	// build there would fail first, on an environment that cannot run one, and
	// the case would say nothing about the ruling it is about.
	var built string
	if os.Getenv(dockerlessEnv) == "" {
		dir, err := buildHost()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		built = dir
	}
	code := m.Run()
	if built != "" {
		os.RemoveAll(built)
	}
	os.Exit(code)
}

// buildHost compiles the host into a directory of its own and reports where it
// put it.
func buildHost() (string, error) {
	dir, err := os.MkdirTemp("", "dbhost")
	if err != nil {
		return "", fmt.Errorf("creating the build directory failed: %w", err)
	}
	hostBinary = filepath.Join(dir, "dbhost")
	ctx, cancel := context.WithTimeout(context.Background(), buildLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "build", "-o", hostBinary, ".").CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("building the host failed: %w\n%s", err, out)
	}
	return dir, nil
}

// outcome is what one run of the host produced.
type outcome struct {
	stdout string
	stderr string
	code   int
}

// dbConfig is the configuration document a case writes, in the shape the
// implementations' schemas declare: each mounts its items under a namespace of
// its own, and a namespace with a dot in it is nested one level per segment.
type dbConfig struct {
	DB dbSection `json:"db"`
}

type dbSection struct {
	Postgres *engineSection `json:"postgres,omitempty"`
	SQLite   *engineSection `json:"sqlite,omitempty"`
}

// engineSection is one implementation's section. The keys are flat inside it,
// which is what an inlined carrier struct decodes from.
type engineSection struct {
	DSN string `json:"dsn,omitempty"`
	// MaxOpenConns is left out unless a case sets it, so that the default is
	// what most cases run with.
	MaxOpenConns int `json:"max-open-conns,omitempty"`
	// EncryptionKey is left out unless a case needs the encryption paths.
	EncryptionKey string `json:"encryption-key,omitempty"`
	// MigrationLockTimeout is declared by an implementation that supplies a
	// mutex, and setting it on one that does not is a startup failure. Only
	// the PostgreSQL cases set it.
	MigrationLockTimeout string `json:"migration-lock-timeout,omitempty"`
}

// writeConfig writes a configuration document and returns the locator the file
// transport serves it under.
func writeConfig(t *testing.T, doc any) string {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling the config document failed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dbhost.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the config document failed: %v", err)
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// runHost runs one scenario to completion.
//
// The environment is exactly what the case gives and never the ambient one: a
// SPEED_DB_E2E_* or DBHOST_* variable on the machine running the tests would
// otherwise change what a case observes.
func runHost(t *testing.T, scenarioName string, args ...string) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()

	cmd := exec.CommandContext(ctx, hostBinary, args...)
	cmd.Env = []string{scenarioEnv + "=" + scenarioName}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running the host failed: %v\nstdout:\n%s\nstderr:\n%s",
				err, stdout.String(), stderr.String())
		}
		code = exit.ExitCode()
	}
	return outcome{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// requireSuccess asserts that the run left cleanly, printing both streams when
// it did not.
func requireSuccess(t *testing.T, got outcome) {
	t.Helper()
	if got.code != 0 {
		t.Fatalf("the host left with status %d\nstdout:\n%s\nstderr:\n%s", got.code, got.stdout, got.stderr)
	}
}

// countOccurrences reports how many lines of a stream carry a piece of text.
func countOccurrences(stream, text string) int {
	n := 0
	for line := range strings.SplitSeq(strings.TrimRight(stream, "\n"), "\n") {
		if strings.Contains(line, text) {
			n++
		}
	}
	return n
}

// requireDisabledWithReason asserts that a startup diagnostic says the named
// module did not run, and returns the line that says it.
//
// The reason is the point of the line and is asserted with the rest of it: a
// diagnostic that named a module without saying what to set would leave the
// operator to guess which of the module's input items matters, and the reason
// names the one whose absence disabled it.
func requireDisabledWithReason(t *testing.T, got outcome, module string, wants ...string) string {
	t.Helper()
	for _, line := range strings.Split(got.stderr, "\n") {
		if !strings.Contains(line, `module "`+module+`"`) {
			continue
		}
		if !strings.Contains(line, "is not enabled") {
			t.Errorf("the diagnostic for %s does not say the module is disabled:\n%s", module, line)
		}
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Errorf("the diagnostic for %s does not name %q:\n%s", module, want, line)
			}
		}
		return line
	}
	t.Fatalf("the startup diagnostics say nothing about %s, and a module that did not run silently is "+
		"what they exist to prevent:\n%s", module, got.stderr)
	return ""
}

// The case that needs no engine of its own: an encrypted field written and read
// back in a process that has done nothing else with the database.

func TestSerializerIsRegisteredBeforeDelivery(t *testing.T) {
	locator := writeConfig(t, dbConfig{DB: dbSection{SQLite: &engineSection{
		DSN:           filepath.Join(t.TempDir(), "dbhost.sqlite"),
		EncryptionKey: "dbhost-e2e-root-key",
	}}})
	got := runHost(t, caseSerializerRoundTrip, "--config="+locator)
	requireSuccess(t, got)

	if n := countOccurrences(got.stdout, roundTripMarker); n != 1 {
		t.Errorf("the encrypted field made the round trip %d times, want 1:\n%s", n, got.stdout)
	}
	// The mutation this case exists for reports itself as GORM refusing to
	// resolve the serializer, so the text is asserted here as well: an
	// implementation that registered it after delivering the handle would
	// otherwise fail for a reason the case does not name.
	if strings.Contains(got.stderr, "invalid serializer type") {
		t.Errorf("the serializer was not registered by the time the dependant was constructed:\n%s", got.stderr)
	}
}

// The two cases about one import pair: both implementations are carried, and the
// configuration decides which of them runs.

func TestBothImplementationsImportedOneConfigured(t *testing.T) {
	locator := writeConfig(t, dbConfig{DB: dbSection{SQLite: &engineSection{
		DSN: filepath.Join(t.TempDir(), "dbhost.sqlite"),
	}}})
	got := runHost(t, caseOneEngineConfigured, "--config="+locator)
	requireSuccess(t, got)

	// The end-to-end form of what separate namespaces are for. Both
	// implementations declare their input items, and the manifest is collected
	// from every registered module, this run's disabled ones included: had the
	// two shared a namespace they would have conflicted while it was being
	// collected, before exclusivity was ever resolved, and this run would have
	// died there rather than starting on the one that was configured.
	requireDisabledWithReason(t, got, "db.postgres", "db.postgres.dsn")
	// The other half: the configured engine has to be the one that ran. A run
	// that disabled both would carry the line above as well, and the dependant
	// requiring the capability is what rules it out — this line is what says
	// which of the two took the requirement up.
	if countOccurrences(got.stderr, `module "db.sqlite"`) != 0 {
		t.Errorf("the configured implementation is reported as not running:\n%s", got.stderr)
	}
}

func TestBothConfiguredIsExclusiveViolated(t *testing.T) {
	// Neither locator has to name a reachable database: two enabled providers
	// are settled during resolution, and no connection is opened before that.
	locator := writeConfig(t, dbConfig{DB: dbSection{
		Postgres: &engineSection{DSN: "postgres://dbhost.invalid:5432/none"},
		SQLite:   &engineSection{DSN: filepath.Join(t.TempDir(), "unused.sqlite")},
	}})
	got := runHost(t, caseBothEnginesConfigured, "--config="+locator)

	if got.code == 0 {
		t.Fatalf("the startup succeeded with both implementations configured and each declaring the "+
			"capability exclusively\nstdout:\n%s\nstderr:\n%s", got.stdout, got.stderr)
	}
	// errors.Is does not survive the process boundary, so the host judged the
	// error itself and printed a fixed word.
	if !strings.Contains(got.stdout, exclusiveViolatedMarker) {
		t.Errorf("the startup failed for another reason than two enabled providers\nstdout:\n%s\nstderr:\n%s",
			got.stdout, got.stderr)
	}
	for _, module := range []string{"db.postgres", "db.sqlite"} {
		if !strings.Contains(got.stderr, module) {
			t.Errorf("the failure does not name %s, and both providers are what has to be settled:\n%s",
				module, got.stderr)
		}
	}
}

func TestNoDSNDisablesAndDependantGetsMissingProvider(t *testing.T) {
	// A document that carries no database section at all, which is a legal
	// assembly: whether a run needs a database is the run's decision, and this
	// one's dependant makes it.
	locator := writeConfig(t, struct{}{})
	got := runHost(t, caseNoEngineConfigured, "--config="+locator)

	if got.code == 0 {
		t.Fatalf("the startup succeeded although a module required the database capability and neither "+
			"implementation was configured\nstdout:\n%s\nstderr:\n%s", got.stdout, got.stderr)
	}
	if !strings.Contains(got.stdout, missingProviderMarker) {
		t.Errorf("the startup failed for another reason than an unmet requirement\nstdout:\n%s\nstderr:\n%s",
			got.stdout, got.stderr)
	}
	// The requirement is what makes the absence fail the startup, and the
	// diagnostics are what makes it fixable: the reason has to reach the error,
	// or the operator is told a capability is missing and not which dial turns
	// it on.
	requireDisabledWithReason(t, got, "db.sqlite", "db.sqlite.dsn")
	if !strings.Contains(got.stderr, "db.sqlite.dsn") {
		t.Errorf("the unmet requirement does not carry the reason the implementation was disabled:\n%s",
			got.stderr)
	}
}

// The concurrency cases: two replicas of one deployment starting at once
// against an empty database.

// applyDuration is how long the migration the replicas apply keeps the applier
// inside its transaction, and therefore how wide the window the other replica has
// to reach its own acquisition in is. It is the pg_sleep in
// migrations/postgres/0001_widgets.sql, and the overlap case asserts the applier
// really spent this long inside, so that a fixture that stopped widening the
// window fails loudly instead of turning the overlap assertion into a coin toss.
const applyDuration = 3 * time.Second

// concurrentRun holds the one pair of replicas both concurrency cases read.
//
// They are one run rather than two because the two properties only mean anything
// together: the division says the replicas were serialised, and the overlap says
// the second one was there while the first was working. A second replica that
// arrived late reads a complete record table and reports noop — with a mutex or
// without one — so a run whose two processes never overlapped proves nothing
// about the mutex and would still pass a division assertion. Reading the overlap
// off a different run than the division would leave exactly that hole open.
var concurrentRun struct {
	once    sync.Once
	results []replicaRun
}

// runReplicas returns the pair both cases read, starting it on first use.
//
// A skip or a fatal inside Do leaves it incomplete, so the second case re-enters
// and reports the same thing rather than reading a run that was never started.
func runReplicas(t *testing.T) []replicaRun {
	t.Helper()
	concurrentRun.once.Do(func() { concurrentRun.results = startReplicas(t) })
	return concurrentRun.results
}

// startReplicas runs the pair, and returns what each of them did.
func startReplicas(t *testing.T) []replicaRun {
	t.Helper()
	// A machine without docker skips here, and under CI the skip is a failure:
	// this is the gate the cross-process mutex is held by, and a gate that is
	// skipped wherever it is meant to run is not a gate.
	dsn := pgtest.Acquire(t)
	rendezvous := t.TempDir()
	roles := []string{"replica-0", "replica-1"}

	// The documents are written before the processes start: t.TempDir and
	// t.Fatalf belong to the test's own goroutine.
	locators := make([]string, len(roles))
	for i, role := range roles {
		locators[i] = replicaConfig(t, dsn, role)
	}

	results := make([]replicaRun, len(roles))
	var running sync.WaitGroup
	for i, role := range roles {
		running.Add(1)
		go func() {
			defer running.Done()
			results[i] = runReplica(role, locators[i], rendezvous)
		}()
	}
	running.Wait()

	for _, result := range results {
		if result.startErr != nil {
			t.Fatalf("%s could not be started: %v", result.role, result.startErr)
		}
		t.Logf("%s exited %d\nstdout:\n%s\nstderr:\n%s", result.role, result.exit, result.stdout, result.stderr)
	}
	return results
}

// replicaConfig writes one replica's configuration.
//
// The locator carries the replica's own application_name, and that is what the
// migration records: one file is applied by one of the two, and the row it
// writes is the only thing about the finished database that says which. The pool
// is held to a single connection, which is the smallest a host can configure and
// therefore the widest statement of what the migration run needs: the mutex, the
// record table and every statement travel on one connection, so a replica that
// needed a second one anywhere would stop here instead of passing quietly.
func replicaConfig(t *testing.T, dsn, role string) string {
	t.Helper()
	return writeConfig(t, dbConfig{DB: dbSection{Postgres: &engineSection{
		DSN:                  dsn + "&application_name=" + role,
		MaxOpenConns:         1,
		MigrationLockTimeout: "1m",
	}}})
}

// replicaRun is what one replica process did.
type replicaRun struct {
	role     string
	exit     int
	stdout   string
	stderr   string
	startErr error
}

// marked reports whether the replica printed a marker.
func (r replicaRun) marked(name string) bool {
	_, err := r.at(name)
	return err == nil
}

// at reads the moment the replica reached a marker.
func (r replicaRun) at(name string) (time.Time, error) {
	for _, line := range strings.Split(r.stdout, "\n") {
		marker, stamp, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || marker != name {
			continue
		}
		moment, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return time.Time{}, fmt.Errorf("the %q marker carries %q, which is not a timestamp: %w",
				name, stamp, err)
		}
		return moment, nil
	}
	return time.Time{}, fmt.Errorf("%s printed no %q marker", r.role, name)
}

// runReplica starts one replica as a separate process and collects what it did.
func runReplica(role, locator, rendezvous string) replicaRun {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, hostBinary, "--config="+locator)
	cmd.Env = []string{
		scenarioEnv + "=" + caseFirstMigration,
		roleEnv + "=" + role,
		rendezvousEnv + "=" + rendezvous,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	run := replicaRun{role: role}
	err := cmd.Run()
	run.stdout, run.stderr = stdout.String(), stderr.String()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		run.exit = exit.ExitCode()
	default:
		run.startErr = err
	}
	return run
}

// divide sorts the replicas by what they reported, and fails when the pair did
// not come out as exactly one applier and one no-op.
func divide(t *testing.T, results []replicaRun) (applier, noop replicaRun) {
	t.Helper()
	var appliers, noops []replicaRun
	for _, result := range results {
		if result.exit != 0 {
			t.Errorf("%s exited %d, and both replicas have to start successfully:\nstdout:\n%s\nstderr:\n%s",
				result.role, result.exit, result.stdout, result.stderr)
		}
		// Both streams, because the driver's own report of the collision does
		// not travel the way this host's errors do: GORM's default logger is
		// what writes a failed statement out, and it writes to standard
		// output, on the same stream the markers arrive on.
		if strings.Contains(result.stderr+result.stdout, "already exists") {
			t.Errorf("%s failed on an object that already exists, which is the collision the migration "+
				"mutex is there to prevent:\nstdout:\n%s\nstderr:\n%s",
				result.role, result.stdout, result.stderr)
		}
		switch {
		case result.marked(markApplied):
			appliers = append(appliers, result)
		case result.marked(markNoop):
			noops = append(noops, result)
		default:
			t.Errorf("%s printed neither %q nor %q, so it never finished its migration:\nstdout:\n%s\nstderr:\n%s",
				result.role, markApplied, markNoop, result.stdout, result.stderr)
		}
	}
	if len(appliers) != 1 || len(noops) != 1 {
		t.Fatalf("%d replicas applied the migration and %d found it already applied, and exactly one of "+
			"each is what the migration mutex has to produce", len(appliers), len(noops))
	}
	return appliers[0], noops[0]
}

// TestConcurrentFirstMigrationElectsOneApplier is the hard gate on the
// cross-process mutex, seen from a real assembly: two hosts, each importing both
// implementations and configured on PostgreSQL, go for the same empty database at
// the same moment after meeting at a rendezvous.
//
// Exactly one of them has to apply the migration, and the other has to find the
// work already done — not fail on an object that already exists, which is what
// happens with no mutex and is the failure a real deployment hits on its first
// multi-replica start.
func TestConcurrentFirstMigrationElectsOneApplier(t *testing.T) {
	applier, noop := divide(t, runReplicas(t))
	t.Logf("%s applied the migration and %s found it already applied", applier.role, noop.role)
}

// TestConcurrentFirstMigrationProvesTheWindowOverlapped asserts, in the same run
// the division is read from, that the two replicas were in flight together.
//
// Without it the gate above is empty rather than merely weak. The fixture only
// guarantees that the applying replica is slow; nothing guarantees that the
// other one was there while it was working. A cold container, the second test
// leg, a slow exec — any of them make the second replica late, and a late
// replica reads a complete record table and prints noop whether there is a mutex
// or not.
func TestConcurrentFirstMigrationProvesTheWindowOverlapped(t *testing.T) {
	results := runReplicas(t)
	applier, noop := divide(t, results)

	startedWaiting, err := noop.at(markStart)
	if err != nil {
		t.Fatalf("reading when %s reached its migration stage: %v", noop.role, err)
	}
	finishedApplying, err := applier.at(markApplied)
	if err != nil {
		t.Fatalf("reading when %s finished applying: %v", applier.role, err)
	}
	if !startedWaiting.Before(finishedApplying) {
		t.Fatalf("%s only reached its migration stage at %s, after %s had finished applying at %s: the two "+
			"never overlapped, so this run proves nothing about the mutex",
			noop.role, startedWaiting.Format(time.RFC3339Nano),
			applier.role, finishedApplying.Format(time.RFC3339Nano))
	}

	// The window has to be the one the fixture put there. A migration that no
	// longer sleeps leaves the replicas racing over milliseconds, and the
	// assertion above would then hold or fail by luck rather than by the mutex.
	startedApplying, err := applier.at(markStart)
	if err != nil {
		t.Fatalf("reading when %s reached its migration stage: %v", applier.role, err)
	}
	if window := finishedApplying.Sub(startedApplying); window < applyDuration {
		t.Fatalf("%s was inside its migration for %s, and the migration is written to hold it for %s: "+
			"the window this case needs is not there", applier.role, window, applyDuration)
	}
}

// dockerlessEnv marks the re-executed child the docker-free case drives. It stays
// outside this host's prefix for the same reason the scenario variables do, and
// TestMain reads it: the child needs no host binary, and building one there would
// fail before the case it was asked for ever ran.
const dockerlessEnv = "SPEED_DB_E2E_DOCKERLESS"

// TestDockerlessProbe is the child half of the case below. It asks pgtest for a
// database on a machine that has no docker on it, which is a skip where CI is
// unset and a failure where it is set. It runs only from that case: started on
// its own it says which case it belongs to.
func TestDockerlessProbe(t *testing.T) {
	if os.Getenv(dockerlessEnv) == "" {
		t.Skip("the child half of TestTheDockerlessSkipUpgradesUnderCI, which runs it on a machine that " +
			"has no docker on it")
	}
	pgtest.Acquire(t)
	t.Errorf("docker was found on a PATH that has none, so this run says nothing about the ruling")
}

// TestTheDockerlessSkipUpgradesUnderCI pins the ruling the two concurrency cases
// rest on: a machine without docker skips the container cases, so that the suite
// runs on a laptop that has none, and under CI the skip becomes a failure —
// because a gate that is skipped wherever it is actually meant to run is not a
// gate.
//
// Both settings are observed, and the reading that was weighed against this one
// — skip everywhere, so that make test behaves the same in every environment —
// is what the second half rules out: under it this gate would pass silently on
// exactly the machine it exists for. What the skip costs instead is an
// environment-dependent `make test`, which the two cases say out loud.
func TestTheDockerlessSkipUpgradesUnderCI(t *testing.T) {
	withoutCI := runDockerlessChild(t, nil)
	if withoutCI.code != 0 {
		t.Errorf("a machine without docker and without CI left with status %d, and the suite is meant to run "+
			"there by skipping:\n%s", withoutCI.code, withoutCI.output())
	}
	if !withoutCI.saidSkip() {
		t.Errorf("the container case neither skipped nor failed for want of docker on a machine without it, "+
			"so this run says nothing about the ruling:\n%s", withoutCI.output())
	}

	underCI := runDockerlessChild(t, []string{"CI=1"})
	if underCI.code == 0 {
		t.Errorf("the container case was skipped under CI, which is the silent pass this ruling exists to "+
			"prevent:\n%s", underCI.output())
	}
	if !underCI.saidSkip() {
		t.Errorf("the run failed for a reason other than the missing docker, so this run says nothing about "+
			"the ruling:\n%s", underCI.output())
	}
}

// dockerlessReason is what pgtest says when there is no docker to run the
// container cases against. It is the reason text rather than the test's outcome
// that the case asserts, so that a child which failed for another reason — a
// binary that would not start, a case that never ran — reads as the case saying
// nothing rather than as the ruling holding.
const dockerlessReason = "docker is not on PATH"

// childRun is what one re-executed child produced.
type childRun struct {
	code   int
	stdout string
	stderr string
}

// output is both streams, for the messages that say what the child was doing.
func (r childRun) output() string { return r.stdout + r.stderr }

// saidSkip reports whether the child reached pgtest's docker-free path.
func (r childRun) saidSkip() bool { return strings.Contains(r.output(), dockerlessReason) }

// runDockerlessChild runs this binary again with an environment that has no
// docker on it, and collects what it did.
//
// The environment is built rather than inherited, and the empty PATH is the
// point: the child is started by absolute path, so it needs nothing on it to
// run, and it must find no docker anywhere. That is also why it builds no host
// binary — see TestMain.
func runDockerlessChild(t *testing.T, extra []string) childRun {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("locating this test binary to re-execute it: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run", "^TestDockerlessProbe$", "-test.v")
	cmd.Env = append([]string{dockerlessEnv + "=1", "PATH=" + t.TempDir()}, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	run := childRun{}
	runErr := cmd.Run()
	run.stdout, run.stderr = stdout.String(), stderr.String()
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		run.code = exit.ExitCode()
	default:
		t.Fatalf("running the child failed: %v\n%s", runErr, run.output())
	}
	return run
}
