.PHONY: all build test clean docker install lint fmt help \
	image-reth test-reth-cgo test-reth-oracle test-reth-boot \
	docker-nethermind docker-nethermind-test test-nethermind-oracle \
	smoke-nethermind smoke-nethermind-spamoor \
	docker-besu docker-besu-test test-besu-oracle \
	smoke-besu smoke-besu-spamoor \
	docker-geth smoke-geth

# Binary name
BINARY=state-actor
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOCLEAN=$(GOCMD) clean
GOGET=$(GOCMD) get
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
	rm -rf dist/

## docker: Build Docker image
docker:
	docker build -t state-actor:latest .
	docker build -t state-actor:$(VERSION) .

## docker-nethermind: Build the Nethermind-capable image (cgo+grocksdb+rocksdb-from-source)
docker-nethermind:
	docker build -f Dockerfile.nethermind -t state-actor-nethermind:latest -t state-actor-nethermind:$(VERSION) .

## docker-nethermind-test: Build the builder stage so we can run cgo_neth go tests inside it
docker-nethermind-test:
	docker build -f Dockerfile.nethermind --target builder -t state-actor-nethermind-builder:latest .

## test-nethermind-oracle: Run the Tier 2 differential oracle (3 CCD-cited golden hashes)
test-nethermind-oracle: docker-nethermind-test
	docker run --rm --entrypoint bash state-actor-nethermind-builder:latest \
	  -c 'cd /app && go test -tags cgo_neth -run TestDifferentialOracle -v ./client/nethermind/...'

## smoke-nethermind: End-to-end smoke — generate a small DB, boot Nethermind 1.37.0, send 100 dev-mode txs
##   Usage: make smoke-nethermind ACCOUNTS=1000 CONTRACTS=100
ACCOUNTS ?= 1000
CONTRACTS ?= 100
SEED ?= 42
SA_DB ?= /tmp/sa-neth-smoke

# Pre-funded smoke addresses. Mirrors the three accounts that used to come
# from testdata/genesis-funded.json (deterministic dev keys 1, 2, 3 from
# eth_sign / spamoor); state-actor now injects them via --inject-accounts
# instead of consuming an external --genesis JSON.
SMOKE_INJECT_ADDRS ?= 0x7e5f4552091a69125d5dfcb7b8c2659029395bdf,0x2b5ad5c4795c026514f8317c7a215e218dccd6cf,0x6813eb9362372eef6200f3b1dbc3f819671cba69
smoke-nethermind: docker-nethermind
	rm -rf $(SA_DB) && mkdir -p $(SA_DB)
	docker run --rm \
	  -v $(SA_DB):/data \
	  -v $(PWD)/client/nethermind/testdata:/test:ro \
	  state-actor-nethermind:latest \
	  --client=nethermind --db=/data \
	  --accounts=$(ACCOUNTS) --contracts=$(CONTRACTS) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	bash $(PWD)/client/nethermind/testdata/validate-big-db.sh $(SA_DB)

## smoke-nethermind-spamoor: Generate a DB, boot Nethermind 1.37.0, then run
##                           spamoor erc20_bloater for 100 blocks of real workload.
##   Usage: make smoke-nethermind-spamoor ACCOUNTS=1000 CONTRACTS=100 [SPAMOOR=/abs/path/spamoor]
##   Pre-req: spamoor binary on PATH (or pass SPAMOOR=/path/to/spamoor).
##            Build: https://github.com/ethpandaops/spamoor → make
SPAMOOR ?= spamoor
smoke-nethermind-spamoor: docker-nethermind
	rm -rf $(SA_DB) && mkdir -p $(SA_DB)
	docker run --rm \
	  -v $(SA_DB):/data \
	  -v $(PWD)/client/nethermind/testdata:/test:ro \
	  state-actor-nethermind:latest \
	  --client=nethermind --db=/data \
	  --accounts=$(ACCOUNTS) --contracts=$(CONTRACTS) --seed=$(SEED) \
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
# Besu targets — see Dockerfile.besu for the RocksDB / grocksdb version pairing.
# ---------------------------------------------------------------------------

## docker-besu: Build the Besu-capable image (cgo+grocksdb+rocksdb-from-source)
docker-besu:
	docker build -f Dockerfile.besu -t state-actor-besu:latest -t state-actor-besu:$(VERSION) .

## docker-besu-test: Build the builder stage so we can run cgo_besu go tests inside it
docker-besu-test:
	docker build -f Dockerfile.besu --target builder -t state-actor-besu-builder:latest .

## test-besu-oracle: Run the differential oracle (Besu genesis1 + genesisNonce golden hashes)
test-besu-oracle: docker-besu-test
	docker run --rm --entrypoint bash state-actor-besu-builder:latest \
	  -c 'cd /app && go test -tags cgo_besu -run TestDifferentialOracle -v ./client/besu/...'

## smoke-besu: End-to-end smoke — generate a small DB, boot hyperledger/besu:25.11.0, send 100 dev-mode txs
##   Usage: make smoke-besu ACCOUNTS=1000 CONTRACTS=100
SA_BESU_DB ?= /tmp/sa-besu-smoke
smoke-besu: docker-besu
	rm -rf $(SA_BESU_DB) && mkdir -p $(SA_BESU_DB)
	docker run --rm \
	  -v $(SA_BESU_DB):/data \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  state-actor-besu:latest \
	  --client=besu --db=/data \
	  --accounts=$(ACCOUNTS) --contracts=$(CONTRACTS) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	bash $(PWD)/client/besu/testdata/validate-big-db-besu.sh $(SA_BESU_DB)

## smoke-besu-spamoor: Generate a DB, boot hyperledger/besu:25.11.0, then run
##                     spamoor erc20_bloater until BLOCKS blocks have been mined.
##   Usage: make smoke-besu-spamoor ACCOUNTS=1000 CONTRACTS=100 BLOCKS=200
##   Pre-req: SPAMOOR=/path/to/spamoor (default /Users/random_anon/dev/spamoor/bin/spamoor)
SPAMOOR ?= /Users/random_anon/dev/spamoor/bin/spamoor
BLOCKS ?= 200
smoke-besu-spamoor: docker-besu
	rm -rf $(SA_BESU_DB) && mkdir -p $(SA_BESU_DB)
	docker run --rm \
	  -v $(SA_BESU_DB):/data \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  state-actor-besu:latest \
	  --client=besu --db=/data \
	  --accounts=$(ACCOUNTS) --contracts=$(CONTRACTS) --seed=$(SEED) \
	  --chain-id=1337 --inject-accounts=$(SMOKE_INJECT_ADDRS) --verbose
	docker rm -f besu-smoke-spamoor 2>/dev/null || true
	docker run --rm -d --name besu-smoke-spamoor \
	  -v $(PWD)/client/besu/testdata:/test:ro \
	  -v $(SA_BESU_DB):/data \
	  -p 127.0.0.1:8545:8545 \
	  hyperledger/besu:25.11.0 \
	  --data-path=/data \
	  --genesis-file=/test/genesis-funded.json \
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

## tidy: Tidy go modules
tidy:
	$(GOMOD) tidy

## deps: Download dependencies
deps:
	$(GOMOD) download

# ---------------------------------------------------------------------------
# Geth targets — pure-Go state-actor build + upstream ethereum/client-go
# image for the bootcheck. Mirrors the docker-{nethermind,besu} pattern.
# ---------------------------------------------------------------------------

## docker-geth: Build the Geth-capable image (state-actor only; no cgo)
docker-geth:
	docker build -f Dockerfile.geth -t state-actor-geth:latest -t state-actor-geth:$(VERSION) .

## smoke-geth: End-to-end smoke for the geth direct-Pebble MPT path.
##   Builds the state-actor-geth image, generates a small DB at
##   $(SA_DB_GETH), then boots upstream ethereum/client-go against the
##   same datadir and runs RPC-based boot-readability checks.
##   Usage: make smoke-geth ACCOUNTS=1000 CONTRACTS=100 SEED=42
SA_DB_GETH ?= /tmp/sa-geth-smoke
GETH_SMOKE_ACCOUNTS ?= 1000
GETH_SMOKE_CONTRACTS ?= 100
GETH_SMOKE_SEED ?= 42
smoke-geth: docker-geth
	rm -rf $(SA_DB_GETH) && mkdir -p $(SA_DB_GETH)/geth/chaindata
	docker run --rm \
	  -v $(SA_DB_GETH):/datadir \
	  -v $(PWD)/client/geth/testdata:/test:ro \
	  state-actor-geth:latest \
	  --client=geth --db=/datadir/geth/chaindata \
	  --accounts=$(GETH_SMOKE_ACCOUNTS) --contracts=$(GETH_SMOKE_CONTRACTS) \
	  --seed=$(GETH_SMOKE_SEED) \
	  --genesis=/test/genesis-funded.json --verbose 2>&1 \
	  | tee $(SA_DB_GETH)/smoke.log
	@expected_root=$$(grep -E '^State Root:' $(SA_DB_GETH)/smoke.log | awk '{print $$NF}'); \
	bash $(PWD)/client/geth/testdata/validate-big-db-geth.sh $(SA_DB_GETH) "$$expected_root"

## example: Run example generation
example:
	./$(BINARY) \
		--db /tmp/example-chaindata \
		--chain-id 1337 \
		--inject-accounts $(SMOKE_INJECT_ADDRS) \
		--accounts 1000 \
		--contracts 500 \
		--max-slots 100 \
		--seed 42 \
		--verbose \
		--benchmark
	@echo ""
	@echo "Example database created at /tmp/example-chaindata"
	@du -sh /tmp/example-chaindata

## help: Show this help
help:
	@echo "State Actor - Ethereum State Generator"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

# ---------------------------------------------------------------------------
# Reth / cgo targets
# ---------------------------------------------------------------------------

## image-reth: Build the cgo+libmdbx Docker image for direct-write reth
image-reth:
	docker build -f Dockerfile.reth --target builder -t state-actor-reth .

## test-reth-cgo: Run cgo_reth-tagged unit tests inside the Docker image
# Use this when local dev does not have libmdbx + librocksdb headers installed.
test-reth-cgo: image-reth
	docker run --rm state-actor-reth go test -tags cgo_reth ./client/reth/...

## test-reth-oracle: Run the differential oracle test (boots paradigmxyz/reth db stats)
# Requires Docker daemon. Gated by build tags `cgo_reth oracle`.
# Uses a named Docker volume so both containers (state-actor-reth and
# paradigmxyz/reth) share the same filesystem namespace via the Docker daemon.
ORACLE_VOL ?= reth-oracle-datadir
test-reth-oracle: image-reth
	docker volume rm -f $(ORACLE_VOL) >/dev/null 2>&1 || true
	docker volume create $(ORACLE_VOL)
	docker run --rm \
	  -v $(ORACLE_VOL):/oracle-data \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -e RETH_ORACLE_DATADIR=/oracle-data \
	  -e RETH_ORACLE_VOL=$(ORACLE_VOL) \
	  state-actor-reth go test -tags 'cgo_reth oracle' ./client/reth/ -run TestRethDbStats -v -timeout 300s
	docker volume rm -f $(ORACLE_VOL) >/dev/null 2>&1 || true

## test-reth-boot: Boot reth node --dev against a state-actor datadir and verify via JSON-RPC
# Slice E deliverable: proves the full direct-write pipeline produces a reth-compatible datadir.
# Requires Docker daemon. Gated by build tags `cgo_reth oracle`.
# Uses a named Docker volume so the test container and the reth container share the same
# filesystem namespace via the Docker daemon socket.
BOOT_VOL ?= reth-boot-datadir
test-reth-boot: image-reth
	docker volume rm -f $(BOOT_VOL) >/dev/null 2>&1 || true
	docker volume create $(BOOT_VOL)
	docker run --rm \
	  -v $(BOOT_VOL):/oracle-data \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -e RETH_ORACLE_DATADIR=/oracle-data \
	  -e RETH_ORACLE_VOL=$(BOOT_VOL) \
	  state-actor-reth go test -tags 'cgo_reth oracle' ./client/reth/ -run TestRethNodeBoot -v -timeout 600s
	docker volume rm -f $(BOOT_VOL) >/dev/null 2>&1 || true
