# The e2e "known Go process". Build context is this directory.
FROM golang:1.26.1 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/goworkload .

FROM scratch
COPY --from=build /out/goworkload /goworkload
ENTRYPOINT ["/goworkload"]
