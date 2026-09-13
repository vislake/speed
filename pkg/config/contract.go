package config

import (
	"context"
	"net/url"
)

// Source is the transport extension point: one implementation per URI scheme.
// An implementation is delivered as a resource, so config finds it by walking
// the registry and never imports it.
type Source interface {
	// Scheme is the URI scheme this transport answers for, without the
	// separator, for example "file". It is registered in lower case, so
	// "FILE" and "file" are one scheme rather than two, and two transports
	// declaring them conflict.
	Scheme() string
	// Fetch reads the primary config source the locator points at and
	// reports the format name of the data it returns. The name is matched
	// exactly against the registered formats; config never sniffs content,
	// because valid JSON is also valid YAML and a wrong guess parses the
	// same bytes under the other grammar. Exactly is about that and about
	// priorities, not about case: the name is folded to lower case before
	// it is matched, so "YAML" reaches the parser registered as yaml.
	//
	// A locator this transport cannot serve, such as a remote host handed to
	// a file source, is an error here rather than a silent fallback.
	Fetch(ctx context.Context, locator *url.URL) (data []byte, format string, err error)
}

// Format is the format extension point: one implementation per format name.
// Like a transport, it is delivered as a resource.
type Format interface {
	// Name is the format name, for example "json". It is registered in
	// lower case, so "JSON" and "json" are one name rather than two, and
	// two parsers declaring them conflict.
	Name() string
	// Unmarshal decodes the data into a tree of string-keyed maps. A
	// non-string key anywhere in the tree, which YAML permits, is an error:
	// silently stringifying it would make 1 and "1" indistinguishable, and a
	// numeric key is not a legal config key in the first place.
	Unmarshal(data []byte) (map[string]any, error)
}

// Reader is how a module reads its own values back. Modules take it up as a
// capability, in Prepare to decide whether they run and in New to build their
// product.
//
// The interface carries reads only: configuration is a snapshot taken at
// startup and does not change afterwards.
type Reader interface {
	// Decode writes the values under a path into target. The path is the
	// complete one in the config data, namespace plus mount path, which the
	// module assembles itself from what it declared.
	//
	// Defaults arrive through the config data's base layer, so a caller
	// passing a zero struct still gets them. A path that was never declared
	// is not an error: target keeps what the caller put in it.
	Decode(path string, target any) error
}
