.PHONY: proto-gen build test test-unit test-integration vet lint clean install

proto-gen:
	protoc --go_out=. --go-grpc_out=. --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative proto/hubfuse.proto

build:
	go build ./...

test: test-unit test-integration

test-unit:
	go test ./internal/...

test-integration:
	go test ./tests/integration/... -timeout 120s

vet:
	go vet ./...

clean:
	rm -f hubfuse-hub hubfuse-agent

install:
	go install ./cmd/hubfuse-hub/ ./cmd/hubfuse-agent/
