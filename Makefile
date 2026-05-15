.PHONY: all build install test coverage coverage-check lint sec secrets check clean upgrade-deps release hooks unhooks tokens

VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")

build:
	go tool goimports -w .
	go build -ldflags "-X main.version=dev -X main.commit=$$(git rev-parse --short HEAD 2>/dev/null || echo none) -X main.date=$$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o bin/loupe ./cmd/loupe

install:
	go install ./cmd/loupe

test:
	go tool gotestsum ./...

coverage:
	go tool gotestsum -- -coverprofile=coverage.out $$(go list ./... | grep -v /cmd/)
	go tool cover -func=coverage.out

coverage-check: coverage
	@go tool cover -func=coverage.out | awk '/^total:/{gsub(/%/,"",$$NF); printf "Total coverage: %s%%\n", $$NF; if ($$NF+0 < 80.0) {print "FAIL: below 80% threshold"; exit 1} else {print "OK: meets 80% threshold"}}'

lint:
	go vet ./...
	go tool staticcheck ./...
	go tool golangci-lint run ./...
	go tool nilaway ./...
	go tool gocyclo -over 15 .

sec:
	go tool gosec ./...
	go tool govulncheck ./...

secrets:
	go tool gitleaks git -v

check: lint sec secrets

clean:
	go clean -cache -i

all: lint sec build

upgrade-deps:
	go get -u ./...
	go mod tidy
	go tool gotestsum ./...

tokens:
	@find . -name '*.go' ! -path './vendor/*' -exec cat {} + | wc -w | awk '{printf "%d words (~%d tokens)\n", $$1, int($$1 * 1.3)}'

hooks:
	git config core.hooksPath .githooks

unhooks:
	git config --unset core.hooksPath

release:
	@test -z "$$(git status --porcelain)" || (echo "error: working tree is dirty" && exit 1)
	@command -v gh >/dev/null || (echo "error: gh CLI required to publish brew formula" && exit 1)
	@echo "Tagging $(VERSION)..."
	git tag -a $(VERSION) -m "Release $(VERSION)"
	git push origin $(VERSION)
	HOMEBREW_TAP_GITHUB_TOKEN=$$(gh auth token) go tool goreleaser release --clean
