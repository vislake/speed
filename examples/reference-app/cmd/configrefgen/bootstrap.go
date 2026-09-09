package main

// bootstrap.go supplies the reference's bootstrap layer: the environment
// variables the reference app's process resolves at startup.
//
// The bootstrap surface of a host is host-authored -- every value is read
// with os.Getenv under per-variable resolution rules (see
// internal/app/server.go's ConfigFromEnv and its siblings), never through a
// declarative schema a tool could walk -- so the generator cannot extract
// per-variable facts from a schema. It instead does two things:
//
//  1. Walks the app's own Go source (internal/app and cmd/server) and
//     extracts the exact inventory of environment variables the app reads:
//     every "APP_..." / "PORT" string literal that is either the value of
//     an Env-named constant or an os.Getenv argument. The inventory is
//     therefore derived from code, never hand-listed.
//  2. Carries a curated table of the per-variable facts the source does not
//     declare -- the value's type, its fallback when unset, whether it
//     holds secret material, and one line on what it configures -- with a
//     coverage gate that makes the table honest: the generator fails unless
//     every inventory key appears in the table exactly once and every table
//     entry is a real inventory key. A new environment variable added to
//     the app, or a table row that names a variable nothing reads, turns
//     the drift gate red instead of silently shipping a stale reference.
//
// The curated facts below were read from the app source (each row's
// declaration site is the source of the deep doc comment; the app's own
// DEPLOY.md and examples/reference-app/.env.example carry the operator text
// this table condenses).

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bootstrapVar is one curated row of the bootstrap layer.
type bootstrapVar struct {
	// env is the environment variable name.
	env string
	// kind is how the value is parsed: "string", "int", "bool" or "hexkey"
	// (a hex-encoded 32-byte key material).
	kind string
	// fallback states what the process uses when the variable is unset or
	// empty, in operator terms.
	fallback string
	// secret marks values that hold secret material. Outputs never print
	// any value, so the flag is annotation only -- it marks which variables
	// a real deployment must feed from a secret store.
	secret bool
	// summary is one line on what the variable configures.
	summary string
	// group orders rows in the rendered reference.
	group string
	// example is the literal value the .env.example renders for the key
	// (the empty string for keys best left unset until needed).
	example string
}

// bootstrapTable is the curated fact table, keyed by env variable name.
var bootstrapTable = map[string]bootstrapVar{
	"APP_DEPLOYMENT_MODE": {
		kind: "string", fallback: "standalone", group: "core",
		summary: "Deployment topology: standalone (default) or distributed; constrains which seam implementations may compose.",
		example: "standalone",
	},
	"PORT": {
		kind: "string", fallback: "8080", group: "core",
		summary: "HTTP listen port.",
		example: "8080",
	},
	"APP_DB_PATH": {
		kind: "string", fallback: "reference-app.db", group: "core",
		summary: "SQLite database file path (relative to the working directory).",
		example: "reference-app.db",
	},
	"APP_ROOT_KEY": {
		kind: "hexkey", fallback: "individual development defaults", secret: true, group: "keys",
		summary: "Single root secret from which the other six keys of this group are derived (HKDF-SHA256, one purpose string per key); an explicitly-set individual key always wins over its derivation.",
		example: "REPLACE_WITH_64_HEX_CHARS",
	},
	"APP_CONFIG_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "Master key the config module seals every Sensitive dynamic-configuration value with; the key that encrypts the configs table can never live in that table.",
	},
	"APP_ORG_INDEX_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "HMAC key org's blind indexer indexes invitation email addresses with; separate from APP_CONFIG_KEY (an AES key must never double as an HMAC key).",
	},
	"APP_NOTIFICATION_INDEX_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "HMAC key the notification module's blind indexers index encrypted contact addresses with; separate from every other key.",
	},
	"APP_PKI_LOCAL_KEY_CIPHER_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "AES key sealing go/pki's LocalSigner private-key column, the key authn's access tokens are ultimately signed with.",
	},
	"APP_AUTHN_BLIND_INDEX_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "HMAC key authn indexes users.email_index/phone_index with; must stay identical across restarts or stored indexes become unfindable.",
	},
	"APP_AUTHN_PII_CIPHER_KEY": {
		kind: "hexkey", fallback: "documented non-secret development default", secret: true, group: "keys",
		summary: "AES key sealing authn's encrypted PII columns (email, phone, TOTP secrets); separate from every other key.",
	},
	"APP_REDIS_ADDR": {
		kind: "string", fallback: "in-process eventbus/kv (Preset default)", group: "seams",
		summary: "Redis address composing real, multi-replica-safe EventBus and KVStore implementations for both seams.",
		example: "localhost:6379",
	},
	"APP_S3_ENDPOINT": {
		kind: "string", fallback: "local-directory objectstore (Preset default)", group: "seams",
		summary: "S3-compatible endpoint; required together with bucket/access-key/secret-key below.",
		example: "http://localhost:9000",
	},
	"APP_S3_BUCKET": {
		kind: "string", fallback: "local-directory objectstore (Preset default)", group: "seams",
		summary: "S3 bucket name for the objectstore seam (required with the S3 group).",
	},
	"APP_S3_ACCESS_KEY": {
		kind: "string", fallback: "local-directory objectstore (Preset default)", group: "seams",
		summary: "S3 access key id for the objectstore seam (required with the S3 group).",
	},
	"APP_S3_SECRET_KEY": {
		kind: "string", fallback: "local-directory objectstore (Preset default)", secret: true, group: "seams",
		summary: "S3 secret key for the objectstore seam (required with the S3 group).",
	},
	"APP_S3_REGION": {
		kind: "string", fallback: "unset (ignored by S3-compatible local servers)", group: "seams",
		summary: "Optional S3 region, used only when the S3 group above is set.",
	},
	"APP_S3_USE_SSL": {
		kind: "bool", fallback: "false", group: "seams",
		summary: "Whether the S3 endpoint speaks TLS (optional; plain HTTP is the local-server default).",
		example: "false",
	},
	"APP_OBJECT_STORE_ROOT": {
		kind: "string", fallback: "throwaway temp directory", group: "seams",
		summary: "Fixed local directory for the objectstore seam, the SurvivesRestart twin of the S3 composition; setting both is refused.",
	},
	"APP_SMTP_HOST": {
		kind: "string", fallback: "console mailer (Preset default)", group: "seams",
		summary: "SMTP host composing a real Mailer for the mailer seam; required together with APP_SMTP_PORT.",
	},
	"APP_SMTP_PORT": {
		kind: "int", fallback: "console mailer (Preset default)", group: "seams",
		summary: "SMTP port for the mailer seam (required with APP_SMTP_HOST).",
	},
	"APP_SMTP_USERNAME": {
		kind: "string", fallback: "no SMTP AUTH (optional)", group: "seams",
		summary: "Optional SMTP AUTH username; AUTH activates only when a username is set.",
	},
	"APP_SMTP_PASSWORD": {
		kind: "string", fallback: "no SMTP AUTH (optional)", secret: true, group: "seams",
		summary: "Optional SMTP AUTH password.",
	},
	"APP_SMS_GATEWAY_URL": {
		kind: "string", fallback: "console SMS sender (standalone) / refused boot (distributed)", secret: true, group: "seams",
		summary: "Endpoint the real HTTP SMS transport posts delivery requests to; the URL is where gateway credentials live.",
	},
	"APP_OTLP_ENDPOINT": {
		kind: "string", fallback: "local exporters (stdout traces/metrics)", group: "observability",
		summary: "OTLP/gRPC endpoint traces and metrics are pushed to; empty keeps go/observability's local exporters.",
		example: "localhost:4317",
	},
	"APP_PUBLIC_ORIGIN": {
		kind: "string", fallback: "http://localhost:<resolved PORT>", group: "observability",
		summary: "This deployment's own public origin, the base URL outbound mail links render when a tenant has no branded host; an unset value ships mail with unreachable localhost links.",
	},
	"APP_TRUSTED_PROXIES": {
		kind: "string", fallback: "no trusted proxies (direct connection address recorded)", group: "network",
		summary: "Comma-separated reverse-proxy addresses this deployment receives requests through; what lets session/login-history records carry the real client address.",
	},
	"APP_READ_FLY_CLIENT_IP": {
		kind: "bool", fallback: "false", group: "network",
		summary: "Declares the proxy is Fly's, authorizing authn to read the single-hop Fly-Client-IP header; only takes effect alongside APP_TRUSTED_PROXIES ('true' with an empty proxy list refuses boot).",
		example: "false",
	},
	"APP_WEB_DIST": {
		kind: "string", fallback: "no frontend served (API only)", group: "serving",
		summary: "Directory the server serves the app's built frontend from; unset serves no frontend at all.",
	},
	"APP_DEMO_USERS_PASSWORD": {
		kind: "string", fallback: "no demo-account seed", group: "demo",
		summary: "Gates the boot-time seed of the three demo user accounts (demo-owner/demo-reader/demo-acme-only); a passphrase only a disposable demo server should carry.",
	},
	"APP_DEMO_PLATFORM_STAFF_PASSWORD": {
		kind: "string", fallback: "no platform-staff seed", group: "demo",
		summary: "Gates the boot-time seed of demo-platform-staff@example.com, the rbac.SystemDomain platform administrator; deliberately separate from APP_DEMO_USERS_PASSWORD.",
	},
	"APP_AI_GATEWAY_IMAGE_BASE_URL": {
		kind: "string", fallback: "no image credential row written", group: "demo",
		summary: "Base URL of the OpenAI-compatible images endpoint the smile-simulation pipeline posts to; set together with the API key pair.",
	},
	"APP_AI_GATEWAY_IMAGE_API_KEY": {
		kind: "string", fallback: "no image credential row written", group: "demo",
		summary: "Key the images endpoint accepts; the demo value is not a secret (a local or CI-only fake provider accepts it), while a real deployment's key travels the same variable.",
	},
	"APP_DISABLE_DEMO_USER_HEADER": {
		kind: "string", fallback: "demo identity headers read (any non-empty value disables them)", group: "demo",
		summary: "Kill switch closing the X-Demo-User/X-Demo-User-Id privilege-escalation hole: any non-empty value makes every route resolve its actor from the verified access token alone.",
	},
	"APP_DISABLE_QUEUE_WORKER": {
		kind: "string", fallback: "queue worker started", group: "demo",
		summary: "Test-only: any non-empty value skips standaloneQueue.Start, so this replica never claims or executes a job itself (the distributed-mode integration proof's other-replica driver).",
	},
	"APP_FAIL_SELF_SERVICE_PROVISION": {
		kind: "int", fallback: "0 (disabled)", group: "demo",
		summary: "Test-and-e2e-only failure injection: a positive N fails the first N self-service provisioning attempts of each account; anything that is not a non-negative whole number refuses boot.",
		example: "0",
	},
}

// extractEnvInventory walks the reference app's non-test Go source and
// returns the set of environment variables it reads: every "APP_..." or
// "PORT" string literal that is the value of an Env-named constant or an
// os.Getenv argument. The surface is the two flat directories that read
// process environment at boot (internal/app's ConfigFromEnv and its app-level
// siblings, plus cmd/server's healthcheck); neither has subdirectories.
func extractEnvInventory(moduleDir string) (map[string]string, error) {
	inventory := map[string]string{} // env name -> declaration site "file:line"
	for _, dir := range []string{
		filepath.Join(moduleDir, "internal/app"),
		filepath.Join(moduleDir, "cmd/server"),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			if err := scanFile(path, inventory); err != nil {
				return nil, err
			}
		}
	}
	return inventory, nil
}

// scanFile records every environment variable declaration or os.Getenv
// call found in one Go file.
func scanFile(path string, inventory map[string]string) error {
	// #nosec G304 -- this generator's whole job is reading the Go source
	// files whose env constants it inventories; the path is a file the
	// walker itself derived inside the repository's internal/app and
	// cmd/server directories, never caller input.
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return err
	}

	record := func(env string, pos token.Pos) {
		if !strings.HasPrefix(env, "APP_") && env != "PORT" {
			return
		}
		loc := fset.Position(pos)
		site := fmt.Sprintf("%s:%d", loc.Filename, loc.Line)
		if _, seen := inventory[env]; !seen {
			inventory[env] = site
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ValueSpec:
			// A constant (or variable) whose value is an "APP_*" literal:
			// the declaration site of an Env-named constant. Also catch the
			// direct os.Getenv("...") literal pattern below via CallExpr.
			if len(node.Values) == 1 {
				if lit, ok := node.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if env, err := strconvUnquote(lit.Value); err == nil {
						record(env, node.Pos())
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Getenv" {
				if len(node.Args) == 1 {
					if lit, ok := node.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if env, err := strconvUnquote(lit.Value); err == nil {
							record(env, node.Pos())
						}
					}
				}
			}
		}
		return true
	})
	return nil
}

// strconvUnquote unquotes one Go string literal without importing
// strconv's full surface for a single call.
func strconvUnquote(lit string) (string, error) {
	if len(lit) >= 2 && lit[0] == '"' && lit[len(lit)-1] == '"' {
		return strings.ReplaceAll(lit[1:len(lit)-1], `\`, `\`), nil
	}
	return "", fmt.Errorf("not a quoted string")
}

// bootstrapRows assembles the ordered bootstrap rows for the reference: one
// per environment variable the app reads, in curated group order, failing
// when the inventory and the curated table disagree (a read the table does
// not cover, or a table entry nothing reads).
func bootstrapRows(moduleDir string) ([]bootstrapVar, []string, error) {
	inventory, err := extractEnvInventory(moduleDir)
	if err != nil {
		return nil, nil, err
	}

	groupOrder := []string{"core", "keys", "seams", "observability", "network", "serving", "demo"}
	groupRank := make(map[string]int, len(groupOrder))
	for i, g := range groupOrder {
		groupRank[g] = i
	}

	var problems []string
	for env := range inventory {
		if _, ok := bootstrapTable[env]; !ok {
			problems = append(problems, fmt.Sprintf("the app reads %s (declared at %s) but the bootstrap table has no row for it; add one", env, inventory[env]))
		}
	}
	for env := range bootstrapTable {
		if _, ok := inventory[env]; !ok {
			problems = append(problems, fmt.Sprintf("the bootstrap table lists %s but no app code reads it; remove the row", env))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, problems, nil
	}

	rows := make([]bootstrapVar, 0, len(bootstrapTable))
	for env, row := range bootstrapTable {
		row.env = env
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		gi, gj := groupRank[rows[i].group], groupRank[rows[j].group]
		if gi != gj {
			return gi < gj
		}
		return rows[i].env < rows[j].env
	})
	return rows, nil, nil
}
