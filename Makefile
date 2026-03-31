GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

# packages to operate on (exclude the mvp package)
PACKAGES := $(shell $(GOCMD) list ./... | grep -v "/mvp$$")
GOSEC_DIRS := $(shell $(GOCMD) list -f '{{.Dir}}' ./... | grep -v "/mvp$$")

DEFAULT_BINARY=wormzy
BINARIES := wormzy rendezvous stuncheck mailbox dashboard relay

all: test build 

deploy: build
	-@sudo systemctl stop wormzy-mailbox.service
	-@sudo systemctl stop wormzy-rendezvous.service
	-@sudo systemctl stop wormzy-relay.service
	@$(MAKE) install
	-@sudo systemctl daemon-reload
	-@sudo systemctl restart wormzy-mailbox.service
	-@sudo systemctl restart wormzy-rendezvous.service
	-@sudo systemctl restart wormzy-relay.service

debug:
	$(GOBUILD) -o $(DEFAULT_BINARY) -gcflags "all=-N -l" -v ./cmd/wormzy

build:
# 	gosec -exclude=G104,G307 $(GOSEC_DIRS)
	@for bin in $(BINARIES); do \
		$(GOBUILD) -o $$bin -v ./cmd/$$bin ; \
	done

test:
	$(GOTEST) -v $(PACKAGES)

.PHONY: test-core
test-core:
	$(GOTEST) ./cmd/wormzy ./internal/ui ./internal/rendezvous ./internal/transport

.PHONY: test-stun
test-stun:
	$(GOTEST) -v ./internal/stun

.PHONY: test-transport
test-transport:
	$(GOTEST) -v ./internal/transport

.PHONY: test-all
test-all: test-core test-transport test-stun

.PHONY: install
install:
	@for bin in $(BINARIES); do \
		tmp="/usr/local/bin/.$$bin.tmp" ; \
		cp ./$$bin "$$tmp" && chmod 0755 "$$tmp" && mv "$$tmp" /usr/local/bin/$$bin ; \
	done


.PHONY: gosec
gosec:
	gosec -exclude=G104,G307 $(GOSEC_DIRS)

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f $(BINARIES)


.PHONY: sec-lint
sec-lint:
	golangci-lint run -v --config .golangci.yml ./...
