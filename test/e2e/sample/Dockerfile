# The e2e "known Go process". Build context is this directory. Symbols are kept
# (no -ldflags "-s -w") so the eBPF profiler can symbolize the captured stacks.
FROM golang:1.26.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/goworkload .

FROM scratch
COPY --from=build /out/goworkload /goworkload
ENTRYPOINT ["/goworkload"]
