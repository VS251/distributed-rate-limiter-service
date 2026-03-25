GO      ?= go
GOFLAGS ?= -race

.PHONY: build test test-integration lint proto clean

build:
	$(GO) build -o bin/server ./cmd/server

test:
	$(GO) test $(GOFLAGS) ./...

# Requires Redis at REDIS_URL (default: redis://localhost:6379).
# Start with: docker compose up -d redis
test-integration:
	$(GO) test $(GOFLAGS) -tags integration ./...

lint:
	golangci-lint run ./...

GOBIN  ?= $(shell go env GOPATH)/bin
PROTOC  = PATH="$(GOBIN):$(PATH)" protoc

# Regenerate gRPC stubs. Requires:
#   brew install protobuf
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
proto:
	buf lint
	$(PROTOC) \
		--proto_path=. \
		--go_out=. \
		--go_opt=paths=source_relative \
		--go-grpc_out=. \
		--go-grpc_opt=paths=source_relative \
		api/ratelimiter/v1/ratelimiter.proto

clean:
	rm -rf bin/
