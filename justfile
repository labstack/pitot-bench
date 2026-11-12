set dotenv-load := true

telemetrygen_version := "v0.157.0"

default: test

build:
    mkdir -p bin
    go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always --dirty 2>/dev/null || echo dev)" -o bin/pitot-bench ./cmd/pitot-bench

[positional-arguments]
hetzner *args:
    scripts/hetzner.sh "$@"

telemetrygen:
    mkdir -p bin
    GOBIN="$(pwd)/bin" go install github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@{{telemetrygen_version}}

test:
    go test ./...
    go vet ./...

smoke: build telemetrygen
    ./bin/pitot-bench run --preset quick --telemetrygen ./bin/telemetrygen

