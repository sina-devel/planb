# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /planb ./cmd/planb

# The postgres image provides pg_dump/pg_dumpall/pg_restore/psql. A recent client can dump
# servers back to 9.2, so use the newest major version you need to support.
FROM postgres:18-alpine
COPY --from=build /planb /usr/local/bin/planb
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/planb"]
CMD ["manager"]
