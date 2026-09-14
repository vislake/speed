package http

import (
	"errors"
	"go/ast"
	"io"
	nethttp "net/http"
	"strings"
	"testing"
)

// thing is the request body type the decode tests target.
type thing struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// rejection is the error a body type's own Validate returns. It is a type of
// its own so that a test can ask for it back with errors.As, which is what the
// design promises the caller can do.
type rejection struct{ Field string }

func (r rejection) Error() string { return "field " + r.Field + " is not acceptable" }

// checkedThing carries its own rule, which is the only way a rule reaches this
// package.
type checkedThing struct {
	Name string `json:"name"`
}

func (c *checkedThing) Validate() error {
	if c.Name == "" {
		return rejection{Field: "name"}
	}
	return nil
}

var _ Validator = (*checkedThing)(nil)

// TestDecodeRejectsUnknownField pins the design's rule. A lenient decode turns
// a misspelled name into a field that was never sent, and the request still
// looks successful.
func TestDecodeRejectsUnknownField(t *testing.T) {
	var got thing
	err := Decode(postWithBody(`{"name":"a","cuont":3}`), &got)
	if err == nil {
		t.Fatal("a body carrying an unknown field was accepted")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("the failure does not match ErrMalformedBody: %v", err)
	}
	if !strings.Contains(err.Error(), "cuont") {
		t.Errorf("the failure %q does not name the unknown field, so the caller cannot say which one it was", err)
	}
}

// TestDecodeHasNoLenientSwitch is the structural half of "there is no switch".
// Asserting only that an unknown field was rejected passes just as well in an
// implementation that has a knob and left it in its strict position, and the
// design says there is to be no knob at all.
func TestDecodeHasNoLenientSwitch(t *testing.T) {
	_, files := parseProductionFiles(t)
	var decode *ast.FuncDecl
	for _, file := range files {
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "Decode" {
				decode = fn
			}
		}
	}
	if decode == nil {
		t.Fatal("no top-level func Decode was found, so nothing below was checked")
	}

	params := decode.Type.Params.List
	count := 0
	for _, p := range params {
		count += max(len(p.Names), 1)
		if _, variadic := p.Type.(*ast.Ellipsis); variadic {
			t.Error("Decode takes a variadic parameter; options passed there are the switch " +
				"the design rules out")
		}
	}
	if count != 2 {
		t.Errorf("Decode takes %d parameters, want exactly 2 (the request and the target)", count)
	}

	calls := 0
	var stack []ast.Node
	ast.Inspect(decode, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DisallowUnknownFields" {
			return true
		}
		calls++
		for _, enclosing := range stack {
			switch enclosing.(type) {
			case *ast.IfStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
				*ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
				t.Errorf("DisallowUnknownFields is called inside a %T, so something decides "+
					"whether to call it. The design allows no such decision", enclosing)
			}
		}
		return true
	})
	if calls != 1 {
		t.Errorf("Decode calls DisallowUnknownFields %d times, want exactly 1 unconditional call", calls)
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	var got thing
	err := Decode(postWithBody(`{"name":"a"}{"name":"b"}`), &got)
	if err == nil {
		t.Fatal("a body carrying a second JSON value was accepted, so the second one was silently dropped")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("the failure does not match ErrMalformedBody: %v", err)
	}
}

// TestDecodeMapsMaxBytesError pins that the limit the outermost layer imposes
// arrives here as its own class. Folded into ErrMalformedBody it would reach
// the client as 400, and a caller that sent a well-formed but oversized body
// would be told its JSON is wrong.
func TestDecodeMapsMaxBytesError(t *testing.T) {
	r := postWithBody(`{"name":"a name far longer than the limit allows"}`)
	r.Body = nethttp.MaxBytesReader(nil, r.Body, 8)
	var got thing
	err := Decode(r, &got)
	if err == nil {
		t.Fatal("a body past the endpoint's limit was accepted")
	}
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("the failure does not match ErrBodyTooLarge: %v", err)
	}
	if errors.Is(err, ErrMalformedBody) {
		t.Error("an oversized body also matches ErrMalformedBody, so the two classes are not separable")
	}
	var maxBytes *nethttp.MaxBytesError
	if !errors.As(err, &maxBytes) {
		t.Error("the underlying *nethttp.MaxBytesError is not reachable through the chain, " +
			"so the limit that was hit cannot be read back")
	}
}

// TestDecodeWrapsValidatorError pins both halves of the promise: the caller can
// classify the failure with errors.Is, and can still reach the body type's own
// error with errors.As.
func TestDecodeWrapsValidatorError(t *testing.T) {
	var got checkedThing
	err := Decode(postWithBody(`{"name":""}`), &got)
	if err == nil {
		t.Fatal("a body its own Validate rejects was accepted")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("the failure does not match ErrValidation: %v", err)
	}
	var own rejection
	if !errors.As(err, &own) {
		t.Fatalf("the body type's own error is not reachable through the chain: %v", err)
	}
	if own.Field != "name" {
		t.Errorf("the recovered error names field %q, want name", own.Field)
	}
}

// TestDecodeCallsValidateOnAWellFormedBody is the positive half. Every
// assertion above would hold in an implementation that never calls Validate at
// all and simply fails.
func TestDecodeCallsValidateOnAWellFormedBody(t *testing.T) {
	var got checkedThing
	if err := Decode(postWithBody(`{"name":"a"}`), &got); err != nil {
		t.Fatalf("a body that passes its own rule was refused: %v", err)
	}
	if got.Name != "a" {
		t.Errorf("the decoded value is %+v, want Name \"a\"", got)
	}
}

// TestDecodeSkipsValidationForATypeWithoutARule covers the ordinary body type:
// this package supplies the mechanism and no rules, so a type that declares
// none is decoded and handed back.
func TestDecodeSkipsValidationForATypeWithoutARule(t *testing.T) {
	var got thing
	if err := Decode(postWithBody(`{"name":"a","count":3}`), &got); err != nil {
		t.Fatalf("a well-formed body was refused: %v", err)
	}
	if got != (thing{Name: "a", Count: 3}) {
		t.Errorf("the decoded value is %+v, want {a 3}", got)
	}
}

// TestDecodeClassifiesAnEmptyBody pins what is decided about an empty body: it
// is classified, whatever the class turns out to be. The design does not rule
// on whether an absent body is a failure at all (see the plan's F12), so the
// expected class is deliberately not written here — what is written is that
// the caller never receives a bare io.EOF, which StatusFor would have to map
// to 500 and which names no fixing action.
func TestDecodeClassifiesAnEmptyBody(t *testing.T) {
	var got thing
	err := Decode(postWithBody(""), &got)
	if err == nil {
		return // a legal outcome under one of the rulings still open
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("an empty body surfaces as a bare io.EOF: %v", err)
	}
	matched := 0
	for name, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			matched++
			t.Logf("an empty body is currently classified as %s", name)
		}
	}
	if matched != 1 {
		t.Errorf("an empty body matches %d of this package's sentinels, want exactly 1: %v", matched, err)
	}
	if StatusFor(err) >= nethttp.StatusInternalServerError {
		t.Errorf("an empty body maps to %d; a body the client chose not to send is not a "+
			"server-side failure", StatusFor(err))
	}
}

// TestDecodeToleratesARequestWithoutABody covers the request net/http hands a
// handler for a GET: Body is nil rather than an empty reader.
func TestDecodeToleratesARequestWithoutABody(t *testing.T) {
	r := postWithBody("")
	r.Body = nil
	var got thing
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Decode panicked on a request without a body: %v", p)
		}
	}()
	_ = Decode(r, &got)
}
