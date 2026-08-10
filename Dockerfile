# Agent image. One binary, two roles (ADR 0009): the same image runs the
# controller (StatefulSet) and the node scanner (DaemonSet); the role is the
# first argument.
FROM golang:1.26.1 AS build
WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# CGO off so the binary is fully static and runs on distroless/scratch.
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/agent ./cmd/agent

# Static distroless base: no shell, no package manager. The node role needs to
# read root-owned /proc/<pid>/exe, so this base runs as root (uid 0); the
# DaemonSet pins the rest of the hardened posture (read-only root filesystem,
# all capabilities dropped, seccomp RuntimeDefault) in deploy/node-daemonset.yaml.
FROM gcr.io/distroless/static:latest
COPY --from=build /out/agent /usr/local/bin/agent
ENTRYPOINT ["/usr/local/bin/agent"]
