GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO_PACKAGE=github.com/jdefrancesco/wormzy/internal/buildinfo
BUILD_LDFLAGS=$(strip $(LDFLAGS) -X $(BUILDINFO_PACKAGE).version=$(VERSION) -X $(BUILDINFO_PACKAGE).commit=$(COMMIT) -X $(BUILDINFO_PACKAGE).buildDate=$(BUILD_DATE))
TEST_TIMEOUT ?= 60s
BIN_DIR=bin
SYSTEMD_DIR=/etc/systemd/system
SYSTEMD_UNITS := wormzy-mailbox.service wormzy-relay.service

# packages to operate on (exclude the mvp package)
PACKAGES := $(shell $(GOCMD) list ./... | grep -v "/mvp$$")
GOSEC_DIRS := $(shell $(GOCMD) list -f '{{.Dir}}' ./... | grep -v "/mvp$$")

DEFAULT_BINARY=$(BIN_DIR)/wormzy
BINARIES := wormzy rendezvous stuncheck mailbox dashboard relay

all: test build 

deploy: install install-units
	-@sudo systemctl daemon-reload
	-@sudo systemctl restart wormzy-mailbox.service
	-@sudo systemctl restart wormzy-rendezvous.service
	-@sudo systemctl restart wormzy-relay.service
	-@sudo systemctl --no-pager --full status wormzy-mailbox.service wormzy-relay.service

debug:
	@mkdir -p $(BIN_DIR)
	$(GOBUILD) -ldflags "$(BUILD_LDFLAGS)" -o $(DEFAULT_BINARY) -gcflags "all=-N -l" -v ./cmd/wormzy

build:
# 	gosec -exclude=G104,G307 $(GOSEC_DIRS)
	@mkdir -p $(BIN_DIR)
	@for bin in $(BINARIES); do \
		$(GOBUILD) -ldflags "$(BUILD_LDFLAGS)" -o $(BIN_DIR)/$$bin -v ./cmd/$$bin ; \
	done

.PHONY: wormzy
wormzy:
	@mkdir -p $(BIN_DIR)
	$(GOBUILD) -ldflags "$(BUILD_LDFLAGS)" -o $(BIN_DIR)/wormzy -v ./cmd/wormzy

test:
	$(GOTEST) -timeout $(TEST_TIMEOUT) -v $(PACKAGES)

.PHONY: test-core
test-core:
	$(GOTEST) -timeout $(TEST_TIMEOUT) ./cmd/wormzy ./internal/ui ./internal/rendezvous ./internal/transport

.PHONY: test-stun
test-stun:
	$(GOTEST) -timeout $(TEST_TIMEOUT) -v ./internal/stun

.PHONY: test-transport
test-transport:
	$(GOTEST) -timeout $(TEST_TIMEOUT) -v ./internal/transport

.PHONY: test-all
test-all: test-core test-transport test-stun

.PHONY: install
install: build
	@for bin in $(BINARIES); do \
		tmp="/usr/local/bin/.$$bin.tmp" ; \
		sudo cp ./$(BIN_DIR)/$$bin "$$tmp" && sudo chmod 0755 "$$tmp" && sudo mv "$$tmp" /usr/local/bin/$$bin ; \
	done

.PHONY: install-units
install-units:
	@for unit in $(SYSTEMD_UNITS); do \
		if [ -f ./deploy/systemd/$$unit ]; then \
			sudo cp ./deploy/systemd/$$unit $(SYSTEMD_DIR)/$$unit ; \
		fi ; \
	done

.PHONY: gosec
gosec:
	gosec -exclude=G104,G307 $(GOSEC_DIRS)

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -rf $(BIN_DIR)


.PHONY: sec-lint
sec-lint:
	golangci-lint run -v --config .golangci.yml ./...
