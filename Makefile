.PHONY: all build test test-race test-coverage bench clean install lint fmt tidy deps help \
	image-reth image-besu image-nethermind \
	docker-nethermind smoke-nethermind smoke-nethermind-spamoor \
	docker-besu smoke-besu smoke-besu-spamoor \
	docker-geth smoke-geth \
	test-besu-suite test-geth-suite test-nethermind-suite test-reth-suite \
	spamoor-install

# Binary name
BINARY=state-actor
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt

# Default target
all: build

## build: Build the binary
build:
	$(GOBUILD) $(LDFLAGS) -o $(BINARY) .

## install: Install to $GOPATH/bin
install:
	$(GOCMD) install $(LDFLAGS) .

## test: Run tests
test:
	$(GOTEST) -v ./...

## test-race: Run tests with race detector
test-race:
	$(GOTEST) -race -v ./...

## test-coverage: Run tests with coverage
test-coverage:
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## bench: Run benchmarks
bench:
	$(GOTEST) -bench=. -benchmem ./generator

## lint: Run linter
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, running go vet instead"; \
		$(GOCMD) vet ./...; \
	fi

## fmt: Format code
fmt:
	$(GOFMT) ./...

## clean: Clean build artifacts
clean:
	$(GOCLEAN)
	rm -f $(BINARY)
	rm -f coverage.out coverage.html
	rm -rf dist/ _artifacts/

## tidy: Tidy go modules
tidy:
	$(GOMOD) tidy

## deps: Download dependencies
deps:
	$(GOMOD) download

# ---------------------------------------------------------------------------
# Default knobs shared across smoke + suite targets.
# ---------------------------------------------------------------------------

TARGET_SIZE ?= 4MB
SEED ?= 42

# Pre-funded smoke addresses. Mirrors the three accounts that used to come
# from testdata/genesis-funded.json (deterministic dev keys 1, 2, 3 from
# eth_sign / spamoor); state-actor injects them via --inject-accounts.
SMOKE_INJECT_ADDRS ?= 0x7e5f4552091a69125d5dfcb7b8c2659029395bdf,0x2b5ad5c4795c026514f8317c7a215e218dccd6cf,0x6813eb9362372eef6200f3b1dbc3f819671cba69

# Spamoor binary, resolved via $PATH. Override to a local checkout via
# `SPAMOOR=/abs/path/spamoor make smoke-besu-spamoor` or `make spamoor-install`
# to build the upstream binary into /usr/local/bin (CI uses the latter).
SPAMOOR ?= spamoor

# Spamoor pinned commit (full SHA preferred; branch names also work).
# CI's actions/cache key derives from this — bump when picking up
# upstream improvements. Note: ethpandaops/spamoor's default branch is
# `master`, not `main` — bare `main` would fail to clone.
SPAMOOR_COMMIT ?= master

## spamoor-install: fetch + build spamoor from github.com/ethpandaops/spamoor.
##   Accepts either a branch name (e.g. master) or a full commit SHA in
##   $(SPAMOOR_COMMIT). Used by CI; local devs typically have their own
##   checkout and can skip this target.
spamoor-install:
	@if command -v $(SPAMOOR) >/dev/null 2>&1; then \
		echo "spamoor already on PATH"; exit 0; \
	fi
	# git clone --branch <sha> doesn't work; init + fetch by SHA does
	# (GitHub allows fetching reachable commits by full SHA).
	rm -rf /tmp/spamoor && mkdir -p /tmp/spamoor
	cd /tmp/spamoor && git init -q && git remote add origin https://github.com/ethpandaops/spamoor
	cd /tmp/spamoor && git fetch --depth=1 origin $(SPAMOOR_COMMIT) && git checkout -q FETCH_HEAD
	cd /tmp/spamoor && $(GOBUILD) -o /usr/local/bin/spamoor ./cmd/spamoor

# ---------------------------------------------------------------------------
# Nethermind targets — see Dockerfile.nethermind for RocksDB / grocksdb pairing.
# ---------------------------------------------------------------------------

## docker-nethermind: Build the runtime image (state-actor + nethermind smoke)
docker-nethermind:
	docker build -f Dockerfile.nethermind -t state-actor-nethermind:latest -t state-actor-nethermind:$(VERSION) .

## image-nethermind: Build the builder stage so we can run cgo_neth go tests inside it.
##   Used by test-nethermind-suite. Also reused by CI's per-job docker build.
image-nethermind:
	docker build -f Dockerfile.nethermind --target builder -t state-actor-nethermind-builder:latest .

## smoke-nethermind: End-to-end smoke — generate a small DB, boot Nethermind, send 100 dev-mode txs
##   Usage: make smoke-nethermind TARGET_SIZE=4MB
SA_DB ?= /tmp/sa-neth-smoke
smoke-nethermind: docker-nethermind
	rm -rf $(SA_DB) && mkdir -p $(SA_DB)
	docker run --rm \
	  -v $(SA_DB):/data \
	  -v $(PWD)/client/nethermind/testdata:/test:ro \
	  state-actor-nethermind:latest \
	  --client=nethermind --db=/data \
	  --target-size=$(TARGET_SIZE) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	bash $(PWD)/client/nethermind/testdata/validate-big-db.sh $(SA_DB)

## smoke-nethermind-spamoor: Generate a DB, boot Nethermind, then run spamoor erc20_bloater
##   Usage: make smoke-nethermind-spamoor TARGET_SIZE=4MB
##   Pre-req: spamoor on $PATH (override via SPAMOOR=/abs/path).
smoke-nethermind-spamoor: docker-nethermind
	rm -rf $(SA_DB) && mkdir -p $(SA_DB)
	docker run --rm \
	  -v $(SA_DB):/data \
	  -v $(PWD)/client/nethermind/testdata:/test:ro \
	  state-actor-nethermind:latest \
	  --client=nethermind --db=/data \
	  --target-size=$(TARGET_SIZE) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	docker rm -f neth-smoke-spamoor 2>/dev/null || true
	docker run --rm -d --name neth-smoke-spamoor \
	  -v $(PWD)/client/nethermind/testdata:/test:ro \
	  -v $(SA_DB):/data \
	  -p 127.0.0.1:8545:8545 \
	  nethermind/nethermind:1.37.0 \
	  --config /test/configs/sa-dev-v2.json --log Info
	@printf 'waiting for Nethermind RPC ' ; \
	  until curl -s -o /dev/null --connect-timeout 1 -X POST -H 'Content-Type: application/json' \
	    --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' http://127.0.0.1:8545; do \
	    printf '.' ; sleep 1 ; \
	  done ; echo ' up'
	SPAMOOR=$(SPAMOOR) bash $(PWD)/client/nethermind/testdata/spamoor-100-blocks.sh ; \
	  rc=$$? ; docker stop neth-smoke-spamoor >/dev/null ; exit $$rc

# ---------------------------------------------------------------------------
# Besu targets — see Dockerfile.besu for the RocksDB / grocksdb pairing.
# ---------------------------------------------------------------------------

## docker-besu: Build the runtime image (state-actor + besu smoke)
docker-besu:
	docker build -f Dockerfile.besu -t state-actor-besu:latest -t state-actor-besu:$(VERSION) .

## image-besu: Build the builder stage so we can run cgo_besu go tests inside it.
##   Used by test-besu-suite. Also reused by CI's per-job docker build.
image-besu:
	docker build -f Dockerfile.besu --target builder -t state-actor-besu-builder:latest .

## smoke-besu: End-to-end smoke — generate a small DB, boot hyperledger/besu, send 100 dev-mode txs
##   Usage: make smoke-besu TARGET_SIZE=4MB
SA_BESU_DB ?= /tmp/sa-besu-smoke
smoke-besu: docker-besu
	rm -rf $(SA_BESU_DB) && mkdir -p $(SA_BESU_DB)
	docker run --rm \
	  -v $(SA_BESU_DB):/data \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  state-actor-besu:latest \
	  --client=besu --db=/data \
	  --target-size=$(TARGET_SIZE) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	bash $(PWD)/client/besu/testdata/validate-big-db-besu.sh $(SA_BESU_DB)

## smoke-besu-spamoor: Generate a DB, boot hyperledger/besu, then run spamoor erc20_bloater
##                     until BLOCKS blocks have been mined.
##   Usage: make smoke-besu-spamoor TARGET_SIZE=4MB BLOCKS=200
##   Pre-req: spamoor on $PATH (override via SPAMOOR=/abs/path).
BLOCKS ?= 200
smoke-besu-spamoor: docker-besu
	rm -rf $(SA_BESU_DB) && mkdir -p $(SA_BESU_DB)
	docker run --rm \
	  -v $(SA_BESU_DB):/data \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  state-actor-besu:latest \
	  --client=besu --db=/data \
	  --target-size=$(TARGET_SIZE) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	docker rm -f besu-smoke-spamoor 2>/dev/null || true
	docker run --rm -d --name besu-smoke-spamoor \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  -v $(SA_BESU_DB):/data \
	  -p 127.0.0.1:8545:8545 \
	  hyperledger/besu:25.11.0 \
	  --data-path=/data \
	  --genesis-file=/data/besu-chainspec.json \
	  --network-id=1337 \
	  --rpc-http-enabled --rpc-http-port=8545 --rpc-http-host=0.0.0.0 \
	  --rpc-http-api=ETH,NET,WEB3,ADMIN,MINER \
	  --rpc-http-cors-origins="*" --host-allowlist="*" \
	  --data-storage-format=BONSAI \
	  --genesis-state-hash-cache-enabled \
	  --min-gas-price=0 \
	  --miner-enabled --miner-coinbase=0x7e5f4552091a69125d5dfcb7b8c2659029395bdf \
	  --logging=INFO
	@printf 'waiting for Besu RPC ' ; \
	  until curl -s -o /dev/null --connect-timeout 1 -X POST -H 'Content-Type: application/json' \
	    --data '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}' http://127.0.0.1:8545; do \
	    printf '.' ; sleep 1 ; \
	  done ; echo ' up'
	BLOCKS=$(BLOCKS) SPAMOOR=$(SPAMOOR) bash $(PWD)/client/besu/testdata/spamoor-blocks-besu.sh ; \
	  rc=$$? ; docker stop besu-smoke-spamoor >/dev/null ; exit $$rc

# ---------------------------------------------------------------------------
# Geth targets — pure-Go state-actor build + upstream ethereum/client-go.
# ---------------------------------------------------------------------------

## docker-geth: Build the Geth-capable image (state-actor only; no cgo)
docker-geth:
	docker build -f Dockerfile.geth -t state-actor-geth:latest -t state-actor-geth:$(VERSION) .

## smoke-geth: End-to-end smoke for the geth direct-Pebble MPT path.
##   Builds the state-actor-geth image, generates a small DB at $(SA_DB_GETH),
##   then boots upstream ethereum/client-go against the same datadir and runs
##   RPC-based boot-readability checks.
##   Usage: make smoke-geth TARGET_SIZE=4MB SEED=42
SA_DB_GETH ?= /tmp/sa-geth-smoke
GETH_SMOKE_TARGET_SIZE ?= 4MB
GETH_SMOKE_SEED ?= 42
smoke-geth: docker-geth
	rm -rf $(SA_DB_GETH) && mkdir -p $(SA_DB_GETH)/geth/chaindata
	docker run --rm \
	  -v $(SA_DB_GETH):/datadir \
	  state-actor-geth:latest \
	  --client=geth --db=/datadir/geth/chaindata \
	  --target-size=$(GETH_SMOKE_TARGET_SIZE) \
	  --seed=$(GETH_SMOKE_SEED) \
	  --chain-id=1337 --fork=osaka --inject-accounts=$(SMOKE_INJECT_ADDRS) \
	  --verbose 2>&1 \
	  | tee $(SA_DB_GETH)/smoke.log
	@expected_root=$$(grep -E '^State Root:' $(SA_DB_GETH)/smoke.log | awk '{print $$NF}'); \
	bash $(PWD)/client/geth/testdata/validate-big-db-geth.sh $(SA_DB_GETH) "$$expected_root"

# ---------------------------------------------------------------------------
# Reth / cgo image — builder-stage only (no separate runtime image today).
# ---------------------------------------------------------------------------

## image-reth: Build the cgo+libmdbx Docker image for direct-write reth
##   Used by test-reth-suite. Also reused by CI's per-job docker build.
image-reth:
	docker build -f Dockerfile.reth --target builder -t state-actor-reth .

# ---------------------------------------------------------------------------
# Per-client end-to-end suite tests.
#
# Each `test-{client}-suite` runs the full pipeline for one client back to
# back inside its builder image: db-gen → writer-check → boot → genesis
# state-root capture → oracle re-query → spamoor (~100 blocks at low gas) →
# post-spamoor RPC re-query. Fail-fast within the suite.
#
# CI uses these as the per-PR gate (one job per client + one cross-client
# aggregator that compares genesis state-roots).
#
# RESULT_DIR controls where each suite's JSON output lands (genesis state-
# root + post-spamoor block + sanity flags); CI uploads the artifact for
# the cross-client aggregator to download.
# ---------------------------------------------------------------------------

RESULT_DIR ?= $(PWD)/_artifacts

# All cgo test suites bind-mount the host's spamoor binary at
# /usr/local/bin/spamoor inside the container (read-only) and override
# the SPAMOOR env var to point at it. The $(shell command -v ...)
# fallback to /dev/null keeps `make` from erroring when SPAMOOR is
# unset on the host — the test then fails loud via REQUIRE_SPAMOOR=1
# rather than silently t.Skipping.

## test-besu-suite: Run the besu end-to-end suite (db-gen → boot → spamoor → re-query)
BESU_SUITE_VOL ?= besu-suite-datadir
test-besu-suite: image-besu
	mkdir -p $(RESULT_DIR)
	docker volume rm -f $(BESU_SUITE_VOL) >/dev/null 2>&1 || true
	docker volume create $(BESU_SUITE_VOL)
	docker run --rm \
	  -v $(BESU_SUITE_VOL):/oracle-data \
	  -v $(RESULT_DIR):/result \
	  -v $(shell command -v $(SPAMOOR) 2>/dev/null || echo /dev/null):/usr/local/bin/spamoor:ro \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -e BESU_ORACLE_DATADIR=/oracle-data \
	  -e BESU_ORACLE_VOL=$(BESU_SUITE_VOL) \
	  -e BESU_DOCKER_PLATFORM \
	  -e RESULT_PATH=/result/besu-result.json \
	  -e SPAMOOR=/usr/local/bin/spamoor \
	  -e REQUIRE_SPAMOOR=1 \
	  state-actor-besu-builder:latest \
	  go test -tags 'cgo_besu oracle' ./client/besu/ -run 'TestE2ESuite|TestDifferentialOracle' -v -timeout 1800s
	docker volume rm -f $(BESU_SUITE_VOL) >/dev/null 2>&1 || true

## test-nethermind-suite: Run the nethermind end-to-end suite
NETH_SUITE_VOL ?= neth-suite-datadir
test-nethermind-suite: image-nethermind
	mkdir -p $(RESULT_DIR)
	docker volume rm -f $(NETH_SUITE_VOL) >/dev/null 2>&1 || true
	docker volume create $(NETH_SUITE_VOL)
	docker run --rm \
	  -v $(NETH_SUITE_VOL):/oracle-data \
	  -v $(RESULT_DIR):/result \
	  -v $(shell command -v $(SPAMOOR) 2>/dev/null || echo /dev/null):/usr/local/bin/spamoor:ro \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -e NETH_ORACLE_DATADIR=/oracle-data \
	  -e NETH_ORACLE_VOL=$(NETH_SUITE_VOL) \
	  -e NETH_DOCKER_PLATFORM \
	  -e RESULT_PATH=/result/nethermind-result.json \
	  -e SPAMOOR=/usr/local/bin/spamoor \
	  -e REQUIRE_SPAMOOR=1 \
	  state-actor-nethermind-builder:latest \
	  go test -tags 'cgo_neth oracle' ./client/nethermind/ -run 'TestE2ESuite|TestDifferentialOracle' -v -timeout 1800s
	docker volume rm -f $(NETH_SUITE_VOL) >/dev/null 2>&1 || true

## test-reth-suite: Run the reth end-to-end suite
RETH_SUITE_VOL ?= reth-suite-datadir
test-reth-suite: image-reth
	mkdir -p $(RESULT_DIR)
	docker volume rm -f $(RETH_SUITE_VOL) >/dev/null 2>&1 || true
	docker volume create $(RETH_SUITE_VOL)
	docker run --rm \
	  -v $(RETH_SUITE_VOL):/oracle-data \
	  -v $(RESULT_DIR):/result \
	  -v $(shell command -v $(SPAMOOR) 2>/dev/null || echo /dev/null):/usr/local/bin/spamoor:ro \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -e RETH_ORACLE_DATADIR=/oracle-data \
	  -e RETH_ORACLE_VOL=$(RETH_SUITE_VOL) \
	  -e RETH_DOCKER_PLATFORM \
	  -e RESULT_PATH=/result/reth-result.json \
	  -e SPAMOOR=/usr/local/bin/spamoor \
	  -e REQUIRE_SPAMOOR=1 \
	  state-actor-reth go test -tags 'cgo_reth oracle' ./client/reth/ -v -timeout 2700s
	docker volume rm -f $(RETH_SUITE_VOL) >/dev/null 2>&1 || true

## test-geth-suite: Run the geth end-to-end suite (pure Go, no Docker build)
test-geth-suite:
	mkdir -p $(RESULT_DIR)
	RESULT_PATH=$(RESULT_DIR)/geth-result.json SPAMOOR=$(SPAMOOR) REQUIRE_SPAMOOR=1 \
	  $(GOTEST) -tags oracle -run TestE2ESuite -v -timeout 1800s ./client/geth/...

## help: Show this help
help:
	@echo "State Actor - Ethereum State Generator"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
