BINARY := git-balai
GO ?= go

.PHONY: all build test lint fmt install clean

all: lint test build

build:
	$(GO) build -o bin/$(BINARY) .

test:
	$(GO) test -race ./...

lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...

fmt:
	gofmt -w .

install:
	$(GO) install .

clean:
	rm -rf bin
