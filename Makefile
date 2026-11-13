.PHONY: all build images test contracts hook-test lint fmt fmt-check clean

MODULE     := github.com/virtUOS/bbb-recap
BINARIES   := recap recap-exporter recap-mock
HOOK_TESTS := $(wildcard hook/post_archive/test/*_test.rb)
# Ruby 3.2 is the Ruby of BBB; 4.0 is the current release line.
RUBY_IMAGE  := docker.io/library/ruby:3.2-slim-bookworm
RUBY4_IMAGE := docker.io/library/ruby:4.0-slim

all: lint test build

# go build stamps vcs.revision only when .git is a directory
# (cmd/go/internal/vcs: RootNames {".git", isDir: true}). In a linked git
# worktree .git is a file, so go walks up to an enclosing checkout, if there is
# one, and stamps that commit and its dirty state.
# There the binaries are built without VCS stamping: labels and recap_build_info
# then say "(devel)"/"unknown" instead of a wrong commit. Clones (CI) are unaffected.
BUILDVCS := $(if $(wildcard .git/HEAD),,-buildvcs=false)

# Static binaries (CGO_ENABLED=0, docs/reference/deployment.md §1): they run on scratch
# and in the Alpine image of tools/contract-env.
build:
	@mkdir -p bin
	@for b in $(BINARIES); do echo "CGO_ENABLED=0 go build -trimpath $(BUILDVCS) -o bin/$$b ./cmd/$$b"; CGO_ENABLED=0 go build -trimpath $(BUILDVCS) -o bin/$$b ./cmd/$$b || exit 1; done

# Images (docs/reference/deployment.md §1): the host-built static binaries copied
# into scratch, never pulling a base image. The labels carry the module
# version and vcs.revision of each binary, the values recap_build_info shows.
# The image is as reproducible as the host toolchain; CI runs this same target
# with the latest Go 1.27 patch (.github/workflows/). The base image is never
# pulled here: pull it first (the image workflow does). IMAGE_TAG builds other
# local tags (e.g. `make images IMAGE_TAG=test`) without replacing :dev.
IMAGE_TAG ?= dev
images: build
	@for i in recap recap-exporter recap-mock; do \
		version=$$(go version -m bin/$$i | awk '$$1 == "mod" { print $$3 }'); \
		revision=$$(go version -m bin/$$i | awk -F'[\t=]' '$$3 == "vcs.revision" { print $$4 }'); \
		echo "podman build --pull=never -f deploy/images/$$i.Containerfile -t localhost/$$i:$(IMAGE_TAG) (version $${version:-unknown}, revision $${revision:-unknown})"; \
		podman build --pull=never -q -f deploy/images/$$i.Containerfile \
			--build-arg VERSION="$${version:-unknown}" --build-arg REVISION="$${revision:-unknown}" \
			-t localhost/$$i:$(IMAGE_TAG) . || exit 1; \
	done

# Unit tests: every package except the contract tests. The contract harness
# (contracts/internal/) is a helper with its own unit tests, so it runs here.
test:
	go test $$(go list ./... | grep -v '^$(MODULE)/contracts/' ) ./contracts/internal/...

# Contract tests. Set RECAP_BIN, RECAP_EXPORTER_BIN or RECAP_CONTRACT_BASE_URL
# in the environment to run them; without them the tests skip and say why.
contracts:
	go test -count=1 -v ./contracts/...

# post_archive hook tests: local ruby if present, else in podman on ruby 3.2
# and then on ruby 4.0 (the images must already be present; they are never
# pulled). No :Z on the volume: label=disable avoids relabelling the tree for
# SELinux on every run.
hook-test:
ifeq ($(HOOK_TESTS),)
	@echo "no hook tests yet"
else
	@if command -v ruby >/dev/null 2>&1; then \
		echo "ruby: $$(command -v ruby)"; \
		ruby -e 'ARGV.each { |f| load f }' $(HOOK_TESTS); \
	else \
		for image in $(RUBY_IMAGE) $(RUBY4_IMAGE); do \
			echo "ruby not on PATH: using podman $$image"; \
			podman run --rm --pull=never --security-opt label=disable -v "$(CURDIR)":/src:ro -w /src $$image \
				ruby -e 'ARGV.each { |f| load f }' $(HOOK_TESTS) || exit 1; \
		done; \
	fi
endif

# gofmt checks tracked and unignored Go files only, so ignored nested checkouts are skipped.
lint:
	@files=$$(git ls-files --cached --others --exclude-standard '*.go'); out=$$([ -z "$$files" ] || gofmt -l $$files); if [ -n "$$out" ]; then echo "gofmt: unformatted files:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck: skipped: not installed"; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint: skipped: not installed"; fi

# Prettier for the Markdown, YAML and JSON files (config .prettierrc.yaml; .prettierignore skips generated
# fixtures and files read byte for byte). Runs through npx, so it needs Node.js; npx downloads the pinned
# release on first use (3.10.1, 2026-11-12, https://www.npmjs.com/package/prettier).
PRETTIER  := npx --yes prettier@3.10.1
FMT_PATHS := README.md AGENTS.md CONTRIBUTING.md SECURITY.md CHANGELOG.md .github .golangci.yml .prettierrc.yaml \
	cmd contracts deploy docs/decisions docs/operations docs/overview.md docs/reference formats hook internal \
	tools
FMT_FILES  = $$(git ls-files --cached --others --exclude-standard -- $(FMT_PATHS) | grep -E '\.(md|json|ya?ml)$$')

fmt:
	$(PRETTIER) --write --log-level warn $(FMT_FILES)

fmt-check:
	$(PRETTIER) --check $(FMT_FILES)

clean:
	rm -rf bin

# Regenerate the synthetic fixtures and goldens under testdata/ (docs/reference/fixtures.md).
.PHONY: fixtures
fixtures:
	tools/fixturegen/generate.sh
