BINARY  ?= codeument
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Tzurrr/codeument/internal/cli.Version=$(VERSION)
GOFLAGS := -trimpath
export CGO_ENABLED=0

.PHONY: build test lint vet fmt integration cross clean relay-dev tidy

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/codeument

test:
	go test -race -count=1 ./...

integration:
	go test -race -count=1 -tags integration ./internal/cli/...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

cross:
	@for os in linux darwin windows; do for arch in amd64 arm64; do \
		ext=""; [ $$os = windows ] && ext=".exe"; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/$(BINARY)-$$os-$$arch$$ext ./cmd/codeument || exit 1; \
	done; done

relay-dev:
	docker compose -f deploy/relay/docker-compose.yml up --build

clean:
	rm -rf bin dist
