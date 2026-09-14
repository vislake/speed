package http

import (
	"errors"
	"fmt"
	nethttp "net/http"
	"strings"
	"testing"
)

// setwiseEngine is a routing engine that judges the set it is joining rather
// than one pattern beside another: it holds at most room patterns and refuses
// whatever arrives once the set is that size, which says nothing about which of
// the mounted ones it cannot stand beside.
//
// It is the shape the design's ruling on attribution exists for. The standard
// library's multiplexer judges pairs, so it can name the pattern a refusal
// clashes with; the seam has to hold for an engine that judges the whole set,
// which may only be able to say that a pattern does not fit. Nothing here
// answers the attribution the seam asks of an implementation subpackage, so a
// refusal of this engine is one that cannot be attributed at all.
type setwiseEngine struct {
	held map[string]bool
	room int
}

func newSetwiseEngine(room int) *setwiseEngine {
	return &setwiseEngine{held: make(map[string]bool, room), room: room}
}

func (e *setwiseEngine) ServeHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	nethttp.NotFoundHandler().ServeHTTP(w, r)
}

func (e *setwiseEngine) Handle(pattern string, h nethttp.Handler) {
	if len(e.held) >= e.room {
		panic(fmt.Sprintf("the pattern %q does not fit beside the %d already mounted",
			pattern, len(e.held)))
	}
	e.held[pattern] = true
}

var _ Engine = (*setwiseEngine)(nil)

// TestRefusalNoSetwiseEngineCanAttributeSaysSo pins the failure the ruling on
// attribution is about: a mount the engine refused, and no pair of patterns
// that reproduces the refusal, so there is no second pattern this module could
// name.
//
// The engine takes two patterns and refuses the third, so every pair of them
// mounts on an engine of its own: the refusal is a judgement about the set, not
// about any pair in it. Working a pair out here would be this module answering
// for the engine — the shape the design rules out, because the mounting point
// sees only that a mount was refused and how the engine reached that verdict is
// the engine's own business.
//
// What the failure has to carry is therefore that the refusal went
// unattributed, and that the attribution is the seam's requirement on the layer
// binding the engine. The assertion on the pair is the other half: naming two
// patterns here would present a guess as the engine's verdict, and it is the
// observation an implementation that fell back to naming the pattern it was
// mounting would move.
func TestRefusalNoSetwiseEngineCanAttributeSaysSo(t *testing.T) {
	logger, _ := newRecordingLogger()
	_, err := assembleEndpointOn(t, newSetwiseEngine(2), testSettings("public"), nil, logger,
		func(e Endpoint) {
			e.Route("GET /one", nethttp.NotFoundHandler())
			e.Route("GET /two", nethttp.NotFoundHandler())
			e.Route("GET /three", nethttp.NotFoundHandler())
		})
	if !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("a refused mount did not report ErrRouteConflict: %v", err)
	}
	mustContain(t, err.Error(), "did not attribute the refusal",
		"that no pair was named for this refusal")
	mustContain(t, err.Error(), "the engine seam asks of an implementation subpackage",
		"that naming the pair is what the seam requires of the layer that binds the engine, "+
			"which is what this failure says is missing")
	mustContain(t, err.Error(), `"GET /three"`,
		"the pattern the engine refused, which is the one thing the mounting point knows")
	mustContain(t, err.Error(), `endpoint "public"`, "the endpoint the refusal is on")

	for _, pair := range []string{
		`"GET /one" and "GET /three"`,
		`"GET /two" and "GET /three"`,
	} {
		if strings.Contains(err.Error(), pair) {
			t.Errorf("the failure states the pair %s, and no pair reproduces this refusal: "+
				"naming one would present a guess as the engine's verdict.\nThe message was: %s",
				pair, err.Error())
		}
	}
}
