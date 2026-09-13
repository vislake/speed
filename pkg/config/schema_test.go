package config

import "testing"

// TestZeroOriginsMeansPrimaryAndEnv pins the reading of the zero value: an
// item that says nothing about its origins takes the primary source and the
// environment, which is what keeps the great majority of items out of Items.
func TestZeroOriginsMeansPrimaryAndEnv(t *testing.T) {
	if got, want := (Item{}).Origins.resolved(), OriginPrimary|OriginEnv; got != want {
		t.Fatalf("the zero Origins resolves to %b, want %b", got, want)
	}
	if (Item{}).Origins.resolved().has(OriginFlag) {
		t.Fatal("the zero Origins reaches the command line, and a flag needs a name nobody gave")
	}

	type carrier struct {
		Addr string
	}
	byPath := expandOne(t, Schema{Mounts: mount(&carrier{})}, "MYAPP")
	item := byPath["addr"]
	if !item.origins.has(OriginPrimary) || !item.origins.has(OriginEnv) {
		t.Fatalf("an item declared without Items carries origins %b, want the primary source and the environment", item.origins)
	}
	if item.envName != "MYAPP_ADDR" {
		t.Fatalf("an item declared without Items reads %q, want MYAPP_ADDR", item.envName)
	}
}

func TestExplicitOriginsReplaceTheDefault(t *testing.T) {
	type carrier struct {
		Addr string
	}
	byPath := expandOne(t, Schema{
		Mounts: mount(&carrier{}),
		Items:  map[string]Item{"addr": {Origins: OriginFlag, FlagName: "addr"}},
	}, "MYAPP")
	if got := byPath["addr"].envName; got != "" {
		t.Fatalf("an item declaring the command line alone reads %q, want no environment variable", got)
	}
}
