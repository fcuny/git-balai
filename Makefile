BINARY := git-balai
GO ?= go

.PHONY: all build test lint fmt install clean release

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

# Tag and push a release: make release VERSION=v0.2.0
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@echo "$(VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "VERSION must look like vX.Y.Z"; exit 1; }
	@test "$$(git rev-parse --abbrev-ref HEAD)" = main || { echo "not on main"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	git fetch origin --tags
	git pull --ff-only
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" || { echo "main does not match origin/main"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null || { echo "tag $(VERSION) already exists"; exit 1; }
	$(MAKE) lint test
	git tag -a $(VERSION) -m "$(VERSION)"
	git push origin $(VERSION)
