VERSION ?= 0.2.0
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%d)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: test vet windows clean

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kura-agent.exe ./cmd/kura-agent

clean:
	rm -rf dist
