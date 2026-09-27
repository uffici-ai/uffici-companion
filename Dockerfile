# The companion as a sidecar image.
#
# 🔴 Built for the pod shape of #2013, and NOT published by this repository: the
# release path is the mirror export (epic #1994 decision 36), not a publish from
# here. `publish.yml` is deliberately not triggered by the `uffici/` tree.
#
# `FROM scratch` is the point, not a flourish. This binary runs inside the
# customer's perimeter holding their agent key, and a customer is invited to
# verify what it does — an image with no shell, no package manager and no libc
# is one where "what else is in here" has a short answer.

# ── Build ───────────────────────────────────────────────────────────────────
FROM golang:1.27.1-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# CGO_ENABLED=0 is what makes the scratch stage possible at all: with cgo the
# binary needs a dynamic loader and libc, and scratch has neither.
# -trimpath keeps build-machine paths out of the binary, which is half of what
# makes a build reproducible; the other half is the pinned toolchain above.
ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags="-s -w \
        -X github.com/uffici-ai/uffici-companion/internal/build.version=${VERSION} \
        -X github.com/uffici-ai/uffici-companion/internal/build.commit=${COMMIT} \
        -X github.com/uffici-ai/uffici-companion/internal/build.buildDate=${BUILD_DATE}" \
      -o /out/uffici ./cmd/uffici

# $UFFICI_HOME, created HERE so it lands in the image owned by the user that
# runs. Docker initialises an anonymous volume from the image's directory, so a
# path that does not exist in `scratch` is created root-owned 0755 instead — and
# the companion's first act, minting its wire token, fails with
# `open /var/lib/uffici/wire-token: permission denied`. A Kubernetes mount over
# the same path needs `fsGroup: 65532` for the same reason.
RUN mkdir -p /out/home && chown 65532:65532 /out/home

# ── Runtime ─────────────────────────────────────────────────────────────────
FROM scratch

# CA certificates for wave 1b, when the companion reaches the agents host over
# HTTPS. Carried now because adding them later means rebuilding the base for a
# reason unrelated to the code.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/uffici /uffici
COPY --from=build --chown=65532:65532 /out/home /var/lib/uffici

# 🔴 A NUMERIC user, not a name. `scratch` has no /etc/passwd, so a named user
# cannot be resolved and Kubernetes' runAsNonRoot check has nothing to read —
# the pod then fails to start with an error about the user, not about us.
USER 65532:65532

# $UFFICI_HOME is the one writable path: the token and the socket live there, so
# it must be a volume when the filesystem is read-only, which it should be.
ENV UFFICI_HOME=/var/lib/uffici \
    UFFICI_WIRE_TRANSPORT=tcp \
    UFFICI_COMPANION_PORT=7411
VOLUME ["/var/lib/uffici"]

# The wire, loopback only. Exposed for the pod's own namespace, never published.
EXPOSE 7411

ENTRYPOINT ["/uffici"]
