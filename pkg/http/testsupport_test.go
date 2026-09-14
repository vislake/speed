package http

import (
	"go/ast"
	"go/parser"
	"go/token"
	nethttp "net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// parseProductionFiles parses this package's production files. The test files
// are excluded: what several tests here pin is the package's own shape, and a
// declaration made by a test is not part of it.
func parseProductionFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory failed: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s failed: %v", name, err)
		}
		if file.Name.Name != "http" {
			t.Fatalf("%s declares package %s, not http", name, file.Name.Name)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no production file was parsed, so every pin built on this would pass vacuously")
	}
	return fset, files
}

// postWithBody builds a request carrying body, the shape a handler sees.
func postWithBody(body string) *nethttp.Request {
	return httptest.NewRequest(nethttp.MethodPost, "/things", strings.NewReader(body))
}

// spyWriter is a nethttp.ResponseWriter that records what was actually done to
// it. A httptest.ResponseRecorder cannot tell "WriteHeader was never called"
// from "WriteHeader(200) was called", and that is exactly the distinction
// WriteJSON's failure path turns on.
type spyWriter struct {
	header      nethttp.Header
	status      int
	wroteHeader bool
	body        []byte
	writeErr    error
}

func newSpyWriter() *spyWriter {
	return &spyWriter{header: nethttp.Header{}}
}

func (s *spyWriter) Header() nethttp.Header { return s.header }

func (s *spyWriter) WriteHeader(status int) {
	s.wroteHeader = true
	s.status = status
}

func (s *spyWriter) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.body = append(s.body, p...)
	return len(p), nil
}

var _ nethttp.ResponseWriter = (*spyWriter)(nil)
