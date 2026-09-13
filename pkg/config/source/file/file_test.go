package file_test

import (
	"context"
	"errors"
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
	_, _, err := fetch(t, "file://host/path/app.yaml")
	if !errors.Is(err, config.ErrMalformedLocator) {
		t.Fatalf("err = %v, want ErrMalformedLocator", err)
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("err = %v, want the text to name the host it refused", err)
	}
}

// TestOpaqueLocatorRejected covers file:app.yaml, which names no path at all.
func TestOpaqueLocatorRejected(t *testing.T) {
	_, _, err := fetch(t, "file:app.yaml")
	if !errors.Is(err, config.ErrMalformedLocator) {
		t.Fatalf("err = %v, want ErrMalformedLocator", err)
	}
}

func TestMissingFile(t *testing.T) {
	_, _, err := fetch(t, "file://"+filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, config.ErrSourceUnavailable) {
		t.Fatalf("err = %v, want ErrSourceUnavailable", err)
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
