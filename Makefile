.PHONY: proto-gen build test test-unit test-integration test-cli test-scenarios test-race vet lint clean install release-snapshot release-check

proto-gen:
	protoc --go_out=. --go-grpc_out=. --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative proto/hubfuse.proto

build:
	go build ./...

test: test-unit test-integration test-cli test-scenarios

test-unit:
	go test ./internal/...

test-integration:
	go test ./tests/integration/... -timeout 180s

test-cli:
	go test ./tests/cli/...

test-scenarios:
	go test ./tests/scenarios/... -timeout 180s

test-race:
	go test -race ./internal/agent/... -timeout 300s

vet:
	go vet ./...

clean:
	rm -f hubfuse-hub hubfuse

install:
	go install ./cmd/hubfuse-hub/ ./cmd/hubfuse/

# release helpers (install goreleaser via: go install github.com/goreleaser/goreleaser/v2@latest)
release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check
