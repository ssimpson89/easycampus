# easycampus-go convenience targets. Run `make help` for a list.
#
# Self-documenting: each target carries a `## description` annotation
# that `make help` extracts via awk. Keep the descriptions one line and
# under ~60 chars so the rendered list stays readable in a narrow term.

.PHONY: help build run test race vet lint clean check

BINARY := bin/easycampus
PKG := ./cmd/easycampus

help: ## list targets with one-line descriptions
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[1m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## build the binary into ./bin/easycampus
	@mkdir -p bin
	go build -o $(BINARY) $(PKG)

run: ## run from source, honouring EASYCAMPUS_* env vars
	go run $(PKG)

test: ## run the unit test suite
	go test ./...

race: ## run the suite under -race (slower; catches data races)
	go test -race -count=1 ./...

vet: ## go vet across all packages
	go vet ./...

lint: ## gofmt check; fails if gofmt finds anything
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

clean: ## remove the build output directory
	rm -rf bin/

check: lint vet race ## CI gate: gofmt + vet + race
