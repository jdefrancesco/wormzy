GOCMD=go 
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

# packages to operate on (exclude the mvp package)
PACKAGES := $(shell $(GOCMD) list ./... | grep -v "/mvp$$")

BINARY_NAME=wormzy

all: test build

debug: 
	$(GOBUILD) -o $(BINARY_NAME) -gcflags "all=-N -l" -v ./cmd/wormzy

build:
	gosec -exclude=G104,G307 $(PACKAGES)
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/${BINARY_NAME}

test:
	$(GOTEST) -v $(PACKAGES)

.PHONY: install
install:
	cp ./$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)


.PHONY: gosec
gosec:
	gosec -exclude=G104,G307 ./...

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
