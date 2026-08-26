# Agent image. One binary, two roles (ADR 0009): the same image runs the
# controller (a Deployment — ADR 0026) and the node scanner (a DaemonSet); the
# role is the first argument.
FROM golang:1.26.1 AS build
WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO off so the binary is fully static and runs on distroless/scratch.
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/agent ./cmd/agent

# Static distroless base: no shell, no package manager.
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
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /usr/local/bin/agent
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/agent"]
