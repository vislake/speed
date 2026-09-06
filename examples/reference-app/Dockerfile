# syntax=docker/dockerfile:1

# examples/reference-app/Dockerfile -- multi-stage build for speed's
# mandatory first consumer (root CLAUDE.md's "Reference App" section).
#
# BUILD CONTEXT MUST BE THE REPOSITORY ROOT, NOT examples/reference-app/:
#
#   docker build -f examples/reference-app/Dockerfile -t reference-app .
#
# run from the repository root (the accompanying docker-compose.yml's own
# build.context does this for you). This app's own go.mod carries a
# `replace` directive for every sibling go/* module it imports, pointing at
# ../../go/<module> by RELATIVE PATH (see that file's own header), and it
# builds under go.work workspace mode -- go.work lists this app alongside
# every go/* module, and Go's workspace-mode auto-detection walks up parent
# directories from the build's working directory looking for a go.work file,
# which is exactly why the build RUN below can `cd` into
# examples/reference-app and still resolve the whole workspace graph. So
# the build stage needs go.work, go.work.sum, every go/* module AND
# examples/reference-app all visible in one context, which only the
# repository root provides -- root CLAUDE.md's own Taskfile.yml note on
# package patterns says the identical thing for `go test`/`go vet`.
#
# CGO_ENABLED=0: go/dbkit's SQLite dialect driver
# (github.com/glebarez/sqlite, wrapping modernc.org/sqlite) is pure Go --
# no cgo anywhere in that dependency chain (verified directly against this
# round's pinned versions: no `import "C"` in either module, only
# incidental hash-listing matches in go.sum) -- so this app's binary is a
# genuinely static executable. That is what lets the final stage below be
# gcr.io/distroless/static rather than a full Linux distribution, or even
# distroless's own glibc-based "base" variant: no libc, no shell, no
# package manager, nothing but the binary, a nonroot user and a CA
# certificate bundle (needed for outbound TLS -- a real SMTP relay,
# S3-compatible endpoint or HTTP SMS gateway, all real possibilities once
# SPEED_SMTP_*/SPEED_S3_*/SPEED_SMS_GATEWAY_URL are set -- see
# cmd/server/server.go).
#
# The go builder image's tag is pinned to go.work's own `go` directive
# (1.26.8) exactly -- bump both together, and re-verify the CGO_ENABLED=0
# claim above whenever the SQLite driver pin changes.
FROM golang:1.26.8-bookworm AS builder

WORKDIR /src

# Workspace files first, module-cache-friendly: these change far less often
# than any module's own source, so a source-only edit downstream reuses the
# module download layer instead of re-fetching everything.
COPY go.work go.work.sum ./

# Every go/* module go.work lists. Workspace mode needs a real go.mod at
# every one of go.work's `use` entries to resolve at all -- a partial copy
# here fails the build with a workspace-configuration error, not a missing-
# package one -- so this stays the whole go/ tree, not just the modules
# this app happens to import today.
COPY go/ ./go/

# The app itself, including its own go.mod/go.sum and the replace
# directives that point back at the go/ modules just copied above.
COPY examples/reference-app/ ./examples/reference-app/

WORKDIR /src/examples/reference-app

# TARGETOS/TARGETARCH are BuildKit's automatic platform build args --
# populated for every build (plain `docker build` included: recent Docker
# builds through buildx by default, which sets these from the current
# builder's native platform even with no --platform flag) and driven
# explicitly by reusable-docker-build.yml's multi-arch matrix
# (linux/amd64, linux/arm64). Declared with no default here; the RUN
# below's own shell parameter expansion supplies linux/amd64 as a fallback
# for the unlikely case either arrives empty, rather than risking a
# Dockerfile-level ARG default silently overriding a real cross-build
# target.
ARG TARGETOS
ARG TARGETARCH

# Go module/build caches persist across invocations via BuildKit cache
# mounts -- a real speedup on a rebuild after a source-only change, since
# the module download and most package compilation results survive.
# -trimpath and the stripped -ldflags keep the resulting binary
# reproducible (no build-path leakage) and smaller. CGO_ENABLED=0 is the
# one flag this whole file's shape depends on -- see the header above.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
    go build -trimpath -ldflags="-s -w" -o /out/reference-app-server ./cmd/server

# /data is where docker-compose.yml mounts the named volume that holds this
# app's SQLite file (SPEED_DB_PATH=/data/reference-app.db) -- created and
# owned by uid/gid 65532 ("nonroot", the final stage's own user, confirmed
# against this exact base image's /etc/passwd) HERE, in the builder stage,
# because the distroless final stage below has no shell, mkdir or chown of
# its own to create or fix up ownership after the fact. Docker initializes
# a freshly-created named volume's ownership and permissions from whatever
# already sits at its mount point in the image at container-create time, so
# this directory's ownership is what makes the volume writable by the
# nonroot user rather than failing every open with a permission-denied
# (empirically confirmed: omitting this step makes dbkit's sqlite driver
# fail with dbkit.connect_failed against a fresh volume, because the volume
# inherits root:root/0755 from Docker's own default and the nonroot process
# can create no file there).
RUN mkdir -p /data && chown 65532:65532 /data

# Runtime stage: distroless/static, nonroot -- no shell, no package
# manager, no libc, just the static binary above, a CA certificate bundle
# (ca-certificates.crt, already present in this image -- outbound TLS
# needs it) and an unprivileged user (uid/gid 65532, "nonroot", already
# declared in this image's own /etc/passwd).
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /out/reference-app-server ./reference-app-server
COPY --from=builder --chown=65532:65532 /data /data

# PORT mirrors cmd/server/server.go's own defaultPort ("8080", used when
# PORT is unset) -- setting it here is documentation as much as default;
# docker-compose.yml also sets it explicitly for the same route.
ENV PORT=8080
EXPOSE 8080

# HEALTHCHECK re-invokes this SAME static binary with the "healthcheck"
# argument (cmd/server/main.go's healthcheckArg) rather than shelling out
# to curl/wget: this image has neither, nor a shell for a CMD-shell-form
# HEALTHCHECK to run in, which is exactly why the exec form below is
# mandatory here, not a style choice. runHealthcheck (main.go) probes this
# same process's own /healthz (server.go's healthzPath, confirmed to
# answer 200 "ok" with no tenant required) over loopback and exits 0/1
# accordingly.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/app/reference-app-server", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/app/reference-app-server"]
