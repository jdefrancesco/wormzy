GOCMD=go 
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

# packages to operate on (exclude the mvp package)
PACKAGES := $(shell $(GOCMD) list ./... | grep -v "/mvp$$")
GOSEC_DIRS := $(shell $(GOCMD) list -f '{{.Dir}}' ./... | grep -v "/mvp$$")

BINARY_NAME=wormzy

all: test build

debug: 
	$(GOBUILD) -o $(BINARY_NAME) -gcflags "all=-N -l" -v ./cmd/wormzy

build:
	gosec -exclude=G104,G307 $(GOSEC_DIRS)
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/${BINARY_NAME}

test:
	$(GOTEST) -v $(PACKAGES)

.PHONY: test-core
test-core:
	$(GOTEST) ./cmd/wormzy ./internal/ui ./internal/rendezvous ./internal/transport

.PHONY: test-stun
test-stun:
	$(GOTEST) -v ./internal/stun

.PHONY: install
install:
	cp ./$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)


.PHONY: gosec
gosec:
	gosec -exclude=G104,G307 $(GOSEC_DIRS)

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
