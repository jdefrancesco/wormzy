GOCMD=go 
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get

BINARY_NAME=wormzy

all: test build

debug: 
	$(GOBUILD) -o $(BINARY_NAME) -gcflags "all=-N -l" -v ./cmd/wormzy

build:
	gosec -exclude=G104,G307 ./...
	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/${BINARY_NAME}

test:
	$(GOTEST) -v ./...

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
