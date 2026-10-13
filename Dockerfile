# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.2

# Toolchain used for development commands (build, test, vet, migrations).
# CGO stays enabled so that `go test -race` works.
FROM golang:${GO_VERSION}-bookworm AS tools
ARG GOOSE_VERSION=v3.29.0
ENV CGO_ENABLED=1 \
    GOPATH=/cache/gopath \
    GOMODCACHE=/cache/mod \
    GOCACHE=/cache/build \
    GOFLAGS=-buildvcs=false \
    HOME=/tmp
RUN GOMODCACHE=/tmp/goose-mod GOCACHE=/tmp/goose-build GOBIN=/usr/local/bin \
        go install github.com/pressly/goose/v3/cmd/goose@${GOOSE_VERSION} \
    && GOMODCACHE=/tmp/goose-mod go clean -modcache \
    && rm -rf /tmp/goose-build \
    && mkdir -p /cache/gopath /cache/mod /cache/build \
    && chmod -R 0777 /cache
WORKDIR /src

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wallet ./cmd/wallet

FROM gcr.io/distroless/static-debian13:nonroot AS runtime
COPY --from=build /out/wallet /wallet
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/wallet"]
