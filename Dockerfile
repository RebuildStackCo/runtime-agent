# Agent image. One binary, two roles (ADR 0009): the same image runs the
# controller (a Deployment — ADR 0026) and the node scanner (a DaemonSet); the
# role is the first argument.
FROM golang:1.27.1@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS build
WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO off so the binary is fully static and runs on distroless/scratch.
# -trimpath so the binary carries no build-machine path, which is one fewer
# difference between a build of ours and a rebuild of the same commit.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/agent ./cmd/agent

# Static distroless base: no shell, no package manager, and six OS packages
# whose versions a scanner can read from /var/lib/dpkg/status.d (ADR 0085).
#
# The image runs as uid 65532, and root is the exception one role asks for by
# name (ADR 0037). The controller needs no privilege at all — it reads the API
# and writes a spool — so it takes the default. The node role must read
# root-owned /proc/<pid>/exe, and CAP_SYS_PTRACE alone does not buy that: a
# capability granted to a non-root process does not survive execve without the
# ambient set, which Kubernetes does not populate. Measured, not assumed — see
# ADR 0037. So the node DaemonSet sets runAsUser: 0 in the chart, where the
# exception is visible in review.
#
# The rest of the hardened posture — read-only root filesystem, all capabilities
# dropped, seccomp RuntimeDefault — is pinned per workload by the chart
# (charts/runtime-agent), and asserted against its rendered output.
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/agent /usr/local/bin/agent
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/agent"]
