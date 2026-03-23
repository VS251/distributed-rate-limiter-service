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

proto:
	bash scripts/gen_proto.sh

clean:
	rm -rf bin/
