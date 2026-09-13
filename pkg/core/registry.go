package core

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
)

// configModuleName is the one name the registry recognises. The module that
// carries it is constructed first, during Prepare, and is an implicit
// dependency of every other module. That is the whole of the special
// treatment, and no other module has any.
const configModuleName = "config"

// ProcessRegistry is the process-level registry. Registrations made in init
// land here, so the available set is decided by what the binary imports.
var ProcessRegistry = New()

// Registry holds registrations, the instances built from them, and the
// lifecycle driver.
//
// The descriptor part is written before Run and read-only afterwards. The
// driver itself is single-goroutine: stages are strictly ordered and callbacks
// within a stage run one at a time.
type Registry struct {
	mu sync.RWMutex

	order []string                 // registration order
	mods  map[string]*registration // by module name

	instances map[string]any // constructed products, by module name
	// lifecycle lists the modules that entered construction, in the order
	// they did. Rollback walks it backwards.
	lifecycle []string

	// enablement holds the verdict resolution reached, keyed by module name.
	// It is nil until resolution has run.
	enablement map[string]Enablement

	running bool
}

// registration is a descriptor plus where it was registered from. The other
// party to a name collision may have been imported by a dependency and be
// absent from the host's own code, so the module name alone cannot locate it.
type registration struct {
	module Module
	site   string // package path of the caller of Register
}

// New returns an independent registry. It does not inherit the registrations
// init made on ProcessRegistry: whatever is to be assembled in it has to be
// registered by hand, which is what makes it the way to assemble a small group
// of modules in a test.
func New() *Registry {
	return &Registry{
		mods:      make(map[string]*registration),
		instances: make(map[string]any),
	}
}

// Register records a module descriptor. Modules call it from their own init.
//
// A duplicate name panics, and so does an illegal token in Requires or
// Provides: both are programming errors, and both surface at the moment of
// registration rather than somewhere downstream.
func (r *Registry) Register(m Module) {
	site := callerPackage(3)
	if m.Name == "" {
		panic(fmt.Sprintf("core: module registered from %s has an empty name", site))
	}
	for i, req := range m.Requires {
		capabilityType(req.Token, fmt.Sprintf("module %q Requires[%d]", m.Name, i))
	}
	for i, prov := range m.Provides {
		capabilityType(prov.Token, fmt.Sprintf("module %q Provides[%d]", m.Name, i))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.mods[m.Name]; ok {
		panic(fmt.Sprintf("core: duplicate module name %q: registered from %s and from %s. "+
			"The available set is decided by imports, so one of the two may have been "+
			"pulled in by a dependency rather than by the host itself",
			m.Name, existing.site, site))
	}
	r.mods[m.Name] = &registration{module: m, site: site}
	r.order = append(r.order, m.Name)
}

// Modules gives the full set of descriptors. A capability with a single
// provider can be traced back to its module from here; several providers give
// only the candidate set, and routing by name belongs to the capability
// interface itself. Cross-cutting mechanisms that collect by type use
// Resources instead.
func (r *Registry) Modules() []Module {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Module, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.mods[name].module)
	}
	return out
}

// Lookup returns the descriptor registered under a name. Descriptors do not
// change once registered, so it is usable in any stage.
func (r *Registry) Lookup(name string) (Module, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.mods[name]
	if !ok {
		return Module{}, false
	}
	return reg.module, true
}

// callerPackage reports the package path of the frame skip levels above
// runtime.Callers, which is the caller of the exported method that asked.
func callerPackage(skip int) string {
	var pcs [1]uintptr
	if runtime.Callers(skip, pcs[:]) == 0 {
		return "unknown package"
	}
	frame, _ := runtime.CallersFrames(pcs[:]).Next()
	if frame.Function == "" {
		return "unknown package"
	}
	// frame.Function reads like "pkg/path.Func" or "pkg/path.(*T).Method";
	// the package path ends at the first dot after the last slash.
	fn := frame.Function
	dir := ""
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		dir, fn = fn[:i+1], fn[i+1:]
	}
	if i := strings.Index(fn, "."); i >= 0 {
		return dir + fn[:i]
	}
	return dir + fn
}
