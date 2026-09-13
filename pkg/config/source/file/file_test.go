package file_test

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
)

// transport builds the source the way the configuration module gets it, out
// of the module's resources, so the tests exercise what a host actually ends
// up with rather than a value this package hands itself.
func transport(t *testing.T) config.Source {
	t.Helper()
	reg := core.New()
	reg.Register(file.Module())
	found := core.Resources[config.Source](reg)
	if len(found) != 1 {
		t.Fatalf("the module declares %d Source resources, want exactly 1", len(found))
	}
	return found[0].Value
}

// fetch runs a locator through the transport.
func fetch(t *testing.T, locator string) ([]byte, string, error) {
	t.Helper()
	parsed, err := url.Parse(locator)
	if err != nil {
		t.Fatalf("the test locator %q does not parse: %v", locator, err)
	}
	return transport(t).Fetch(context.Background(), parsed)
}

// write puts a file with the given base name in a temporary directory and
// gives back the locator that points at it.
func write(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	return "file://" + path
}

func TestScheme(t *testing.T) {
	if got := transport(t).Scheme(); got != "file" {
		t.Errorf("Scheme() = %q, want %q", got, "file")
	}
}

// TestFormatByExtension pins the mapping the design fixes: the extension is
// the only evidence of the format, and .yaml and .yml name the same one.
func TestFormatByExtension(t *testing.T) {
	cases := map[string]string{
		"app.json": "json",
		"app.yaml": "yaml",
		"app.yml":  "yaml",
		"app.JSON": "json",
		"app.YAML": "yaml",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			data, format, err := fetch(t, write(t, name, "content of "+name))
			if err != nil {
				t.Fatalf("Fetch failed: %v", err)
			}
			if format != want {
				t.Errorf("format = %q, want %q", format, want)
			}
			if string(data) != "content of "+name {
				t.Errorf("data = %q, want the file content verbatim", data)
			}
		})
	}
}

// TestUnknownExtension covers the two shapes that leave the format
// undetermined. The file is absent in both, which pins the order as well: the
// extension decides before anything is read, so an unreadable name fails on
// the format rather than on the missing file.
func TestUnknownExtension(t *testing.T) {
	for _, name := range []string{"app.conf", "app"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := fetch(t, "file://"+filepath.Join(t.TempDir(), name))
			if !errors.Is(err, config.ErrUndeterminedFormat) {
				t.Fatalf("err = %v, want ErrUndeterminedFormat", err)
			}
			if errors.Is(err, config.ErrSourceUnavailable) {
				t.Errorf("err = %v, want the format decided before the file is read", err)
			}
		})
	}
}

// TestRemoteHostRejected covers a shape this transport does not serve. It is
// a malformed locator rather than an unavailable source: retrying never helps
// and the host has to fix what it wrote.
func TestRemoteHostRejected(t *testing.T) {
	// file://./app.yaml is the shape that would slip through if accepting
	// localhost were read as accepting a host at all: "." is a host here, and
	// the path it leaves behind is not the one the writer meant.
	for _, locator := range []string{"file://host/path/app.yaml", "file://./app.yaml"} {
		_, _, err := fetch(t, locator)
		if !errors.Is(err, config.ErrMalformedLocator) {
			t.Fatalf("%s gave err = %v, want ErrMalformedLocator", locator, err)
		}
		if !strings.Contains(err.Error(), "host") {
			t.Errorf("%s gave err = %v, want the text to name the host it refused", locator, err)
		}
	}
}

// TestLocalhostHostAccepted covers the host RFC 8089 makes equal to an empty
// one: it names this machine, which is the machine this transport reads.
func TestLocalhostHostAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte("addr: :8080\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	data, format, err := fetch(t, "file://localhost"+path)
	if err != nil {
		t.Fatalf("reading a localhost locator failed: %v", err)
	}
	if format != "yaml" || string(data) != "addr: :8080\n" {
		t.Fatalf("the locator gave format %q and %q, want the file's own", format, string(data))
	}
}

// TestOpaqueLocatorIsARelativePath covers file:app.yaml, the one shape a URI
// has for a path relative to the process working directory.
func TestOpaqueLocatorIsARelativePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte("addr: :8080\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	t.Chdir(dir)
	data, format, err := fetch(t, "file:app.yaml")
	if err != nil {
		t.Fatalf("reading a relative locator failed: %v", err)
	}
	if format != "yaml" || string(data) != "addr: :8080\n" {
		t.Fatalf("the locator gave format %q and %q, want the file's own", format, string(data))
	}
}

// TestOpaqueLocatorDecodesPercentEscapes pins the two shapes to one reading of
// the same name: url.Parse decodes Path and leaves Opaque as written, so the
// transport has to undo the escapes itself or a space would reach the
// filesystem as three characters.
func TestOpaqueLocatorDecodesPercentEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "my app.yaml"), []byte("addr: :8080\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	t.Chdir(dir)
	if _, _, err := fetch(t, "file:my%20app.yaml"); err != nil {
		t.Fatalf("reading an escaped relative locator failed: %v", err)
	}
}

// TestOpaqueLocatorKeepsExtensionRules pins that the relative shape changes
// where the path comes from and nothing else: the extension still names the
// format, and an unrecognised one is still undetermined.
func TestOpaqueLocatorKeepsExtensionRules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte("addr: :8080\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture failed: %v", err)
	}
	t.Chdir(dir)
	_, _, err := fetch(t, "file:app.conf")
	if !errors.Is(err, config.ErrUndeterminedFormat) {
		t.Fatalf("err = %v, want ErrUndeterminedFormat", err)
	}
}

// TestMissingFile also pins that the read error stays on the chain. A host
// telling "the file is not there" apart from "the file would not open" is the
// difference between writing one and fixing permissions, and the only thing
// that carries it is the wrapped fs.ErrNotExist -- which a %v in the wrapping
// verb would have flattened into text nobody can match on.
func TestMissingFile(t *testing.T) {
	_, _, err := fetch(t, "file://"+filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, config.ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want the absent file to stay reachable as fs.ErrNotExist", err)
	}
}

func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which reads a file whatever its mode says")
	}
	locator := write(t, "app.yaml", "a: 1\n")
	path := locator[len("file://"):]
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, _, err := fetch(t, locator)
	if !errors.Is(err, config.ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want the refused open to stay reachable as fs.ErrPermission", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, which claims the file is absent when it is unreadable", err)
	}
}

// TestRegistersAsSourceResource proves the blank-import contract: importing
// the package puts the transport in the process registry, where the
// configuration module finds it by resource type alone.
func TestRegistersAsSourceResource(t *testing.T) {
	var found []core.Resource[config.Source]
	for _, res := range core.Resources[config.Source](core.ProcessRegistry) {
		if res.Module == "config.source.file" {
			found = append(found, res)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the process registry holds %d Source resources from config.source.file, want 1", len(found))
	}
	if got := found[0].Value.Scheme(); got != "file" {
		t.Errorf("the registered transport answers for %q, want %q", got, "file")
	}
}
