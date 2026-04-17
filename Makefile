.PHONY: proto-gen build test test-unit test-integration test-cli vet lint clean install

proto-gen:
	protoc --go_out=. --go-grpc_out=. --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative proto/hubfuse.proto

build:
	go build ./...

test: test-unit test-integration test-cli

test-unit:
	go test ./internal/...

test-integration:
	go test ./tests/integration/... -timeout 120s

test-cli:
	go test ./tests/cli/...

vet:
	go vet ./...

clean:
	rm -f hubfuse-hub hubfuse

install:
	go install ./cmd/hubfuse-hub/ ./cmd/hubfuse/
