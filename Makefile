.PHONY: proto-gen build test test-unit test-integration test-cli test-scenarios test-race vet lint clean install release-snapshot release-check

proto-gen:
	protoc --go_out=. --go-grpc_out=. --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative proto/hubfuse.proto

build:
	go build ./...

test: test-unit test-integration test-cli test-scenarios

test-unit:
	go test ./internal/...

# 300s, not 180s: the old-hub compatibility test (issue #78) is timer-dominated
# and costs ~34s on any machine — grpc-go clamps a client's keepalive interval
# to a 10s floor (internal.KeepaliveMinPingTime), so the ~30s it takes a pre-#72
# hub to punish an idle connection cannot be shortened. That put the package at
# ~103s against a 180s limit, which is a margin thin enough to fail on a slow
# runner rather than a bound.
test-integration:
	go test ./tests/integration/... -timeout 300s

test-cli:
	go test ./tests/cli/...

test-scenarios:
	go test ./tests/scenarios/... -timeout 180s

test-race:
	go test -race ./internal/agent/... -timeout 300s

vet:
	go vet ./...

# `lint` has been in .PHONY since the beginning with no recipe behind it, so
# `make lint` silently did nothing. This is the invocation the CI Lint job
# runs (golangci-lint-action calls `golangci-lint run` with no args); needs
# golangci-lint v2 — the v2 config file is rejected outright by a v1 binary.
lint:
	golangci-lint run

clean:
	rm -f hubfuse-hub hubfuse

install:
	go install ./cmd/hubfuse-hub/ ./cmd/hubfuse/

# release helpers (install goreleaser via: go install github.com/goreleaser/goreleaser/v2@latest)
release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check
