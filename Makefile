VERSION ?= 0.5.0
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%d)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: test vet windows linux deb clean

test:
	go test ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/kura-agent.exe ./cmd/kura-agent
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS) -H=windowsgui" -o dist/kura-agentw.exe ./cmd/kura-agent

linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/linux/kura-agent ./cmd/kura-agent

# Paquet Debian (Ubuntu) : dist/linux/kura-agent_<version>_amd64.deb (dpkg-deb requis).
deb: linux
	sh scripts/build-deb.sh $(VERSION) amd64 dist/linux/kura-agent dist/linux

clean:
	rm -rf dist
