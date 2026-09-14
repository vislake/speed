package http

import (
	"log/slog"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// The two tests here pin the boundary of the self-referential constraint the
// design settled.
//
// A layer that stands for the capability it names draws no edge to itself — a
// self loop is unsatisfiable by construction — so that part of the declaration
// places nothing, and the layer is reported with a cause of its own: nobody is
// absent from the assembly, the declaration is what is wrong. The report is
// there because the declaration is the only place the position it believes it
// has is written down.
//
// Where another layer stands for that capability as well, that part of the
// claim holds: the edge to the other layer is drawn, the constraint places the
// layer, and there is nothing to report. Reporting it anyway sends the reader
// after a declaration that did what it said, which is why the boundary is
// pinned in both directions rather than only where the report is due.

// TestSelfReferentialConstraintIsReportedUnderItsOwnCause pins the reporting
// half, through the surface a registrant reaches: a layer registered with Use
// that delivers a capability and declares itself inside it, with nothing else
// on the endpoint standing for that capability.
//
// The assembly has to carry on and run the layer — an absent capability is a
// legal configuration — and the diagnostic has to say which of the two causes
// this is. A chain that dropped the constraint in silence would leave the layer
// believing it has a position it does not have, and one that read it as an
// absent module would send the reader looking for a module that is not the
// problem.
func TestSelfReferentialConstraintIsReportedUnderItsOwnCause(t *testing.T) {
	tr := &trace{}
	logger, records := newRecordingLogger()
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{
			Name:     "recover",
			Provides: []core.Token{(*capRecovery)(nil)},
			After:    []core.Token{(*capRecovery)(nil)},
			Wrap:     noteLayer(tr, "recover"),
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	var reported []string
	for _, line := range records.at(slog.LevelWarn) {
		if strings.Contains(line, "middleware=recover") {
			reported = append(reported, line)
		}
	}
	if len(reported) != 1 {
		t.Fatalf("the constraint of a layer standing only for itself was reported as %v, "+
			"want exactly one diagnostic for it", reported)
	}
	mustContain(t, reported[0], "cause="+string(causeSelfOnly),
		"that the declaring layer is the only layer standing for the capability, which is "+
			"corrected by rewriting the declaration rather than by finding a module")
	mustContain(t, reported[0], typeName(reflect.TypeOf((*capRecovery)(nil)).Elem()),
		"the capability the declaration names")
	mustContain(t, reported[0], "After[0]", "which declaration on that layer placed nothing")
	mustContain(t, reported[0], "endpoint=public", "the endpoint the declaration was made on")

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))
	if got := tr.seen(); !equalStrings(got, []string{"recover"}) {
		t.Errorf("the chain ran %v, and a constraint that placed nothing leaves the layer "+
			"running where Order puts it", got)
	}
}

// TestSelfReferentialConstraintWithAnotherProviderIsNotReported pins the other
// side of the boundary: the layer stands for the capability it names, and so
// does another layer on the endpoint. The edge to itself is dropped as before,
// but the edge to that other layer is drawn and the constraint places the
// layer, so the declaration did what it said and there is nothing to report.
//
// Reporting it would be the report adding a line for a constraint that worked,
// which is exactly what makes the lines for the ones that did not stop being a
// signal.
//
// The Order values are the wrong way round on purpose: left to Order alone
// "warden" comes out first, so the chain shows whether the constraint bound to
// the other provider or was dropped along with the self edge.
func TestSelfReferentialConstraintWithAnotherProviderIsNotReported(t *testing.T) {
	tr := &trace{}
	logger, records := newRecordingLogger()
	handler := mustAssemble(t, testSettings("public"), nil, logger, func(e Endpoint) {
		e.Use(Middleware{
			Name:     "warden",
			Order:    0,
			Provides: []core.Token{(*capAuth)(nil)},
			After:    []core.Token{(*capAuth)(nil)},
			Wrap:     noteLayer(tr, "warden"),
		})
		e.Use(Middleware{
			Name:     "auth",
			Order:    10,
			Provides: []core.Token{(*capAuth)(nil)},
			Wrap:     noteLayer(tr, "auth"),
		})
		e.Route("GET /things", nethttp.NotFoundHandler())
	})

	for _, line := range records.at(slog.LevelWarn) {
		if strings.Contains(line, "middleware=warden") {
			t.Errorf("the constraint of warden was reported as landing on nothing, and another "+
				"layer on this endpoint stands for the capability it names, so the edge to that "+
				"one places it:\n%s", line)
		}
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(nethttp.MethodGet, "/things", nil))
	if got, want := tr.seen(), []string{"auth", "warden"}; !equalStrings(got, want) {
		t.Errorf("the chain ran %v, want %v: warden declared itself inside the capability it "+
			"shares with auth, so the constraint places it there however Order would order the two",
			got, want)
	}
}
