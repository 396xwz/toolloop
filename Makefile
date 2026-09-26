BIN := toolloop
WINDOWS_BIN ?= toolloop.exe
CMD := ./cmd/toolloop
GO ?= go
PYTHON ?= python3
MODEL ?= qwen2.5:14b
INDEX_PATH ?= .
FENCE_NET  := toolloop-fence
FENCE_SUB  := 192.168.100.0/24
FENCE_GW   := 192.168.100.1
SERVER_DEST := 10.0.0.36
SERVER_PORT := 8080

.DEFAULT_GOAL := help

.PHONY: help build build-windows run repl ollama test vet fmt tidy check-services setup-python setup-node index clean docker-run-fenced unfence

help:
	@printf '%s\n' \
		'Toolloop targets:' \
		'  make build                 Builds linux and windows ' \
		'  make build-linux           Builds linux ./toolloop' \
		'  make build-windows         Cross-build a Windows amd64 binary' \
		'  make run                   Run the default llama.cpp backend' \
		'  make repl                  Start the default llama.cpp REPL' \
		'  make ollama                Start an Ollama REPL (MODEL=qwen2.5:14b)' \
		'  make test                  Run Go tests' \
		'  make vet                   Run go vet' \
		'  make fmt                   Format Go source files' \
		'  make tidy                  Tidy Go module dependencies' \
		'  make check-services        Check llama.cpp and Ollama endpoints' \
	' make setup-python Create .venv and install requirements' \
	' make setup-node Install Node/Playwright scrape backend' \
		'  make index INDEX_PATH=dir  Index a directory into RAG' \
		'  make docker-run-fenced     Run container, egress fenced to backend (sudo)' \
		'  make fence-clean           Remove iptables fence rules + network' \
		'  make clean                 Remove generated local artifacts'

build: build-linux build-windows
build-linux:
	$(GO) build -o $(BIN) $(CMD)

build-windows:
	GOOS=windows GOARCH=amd64 $(GO) build -o $(WINDOWS_BIN) $(CMD)

docker-build: build-linux
	docker build -t toolloop .

docker-run:
	docker run -it --rm   --name toolloop   --memory 2g --cpus 2   --pids-limit 512   --read-only   -e HOME=/tmp   -v $$PWD:/work   -v sandbox-tmp:/tmp   toolloop:latest   bash -c 'toolloop --skip-index --server http://$(SERVER_DEST):$(SERVER_PORT) -repl'

# Runs the toolloop container on a dedicated bridge network whose egress is
# fenced by iptables to ONLY reach the LLM backend (SERVER_DEST:SERVER_PORT).
# The iptables step needs root (sudo prompts once). Teardown: make unfence.
docker-run-fenced:
	@echo "[fence] network=$(FENCE_NET) subnet=$(FENCE_SUB) allow=$(SERVER_DEST):$(SERVER_PORT)"
	docker network inspect $(FENCE_NET) >/dev/null 2>&1 || docker network create --driver bridge --subnet $(FENCE_SUB) --gateway $(FENCE_GW) $(FENCE_NET)
	sudo iptables -C FORWARD -s $(FENCE_SUB) -d $(SERVER_DEST) -p tcp --dport $(SERVER_PORT) -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD 1 -s $(FENCE_SUB) -d $(SERVER_DEST) -p tcp --dport $(SERVER_PORT) -j ACCEPT
	sudo iptables -C FORWARD -s $(FENCE_SUB) -j DROP 2>/dev/null || sudo iptables -I FORWARD 2 -s $(FENCE_SUB) -j DROP
	docker run -it --rm   --name toolloop   --network $(FENCE_NET)   --memory 2g --cpus 2   --pids-limit 512   --read-only   -e HOME=/tmp   -v $$PWD:/work   -v sandbox-tmp:/tmp   toolloop:latest   bash -c 'toolloop --skip-index --server http://$(SERVER_DEST):$(SERVER_PORT) -repl'

# Removes the iptables fence rules and the dedicated bridge network.
# Run this if you Ctrl+C out of docker-run-fenced (otherwise the rules persist).
fence-clean:
	-sudo iptables -D FORWARD -s $(FENCE_SUB) -d $(SERVER_DEST) -p tcp --dport $(SERVER_PORT) -j ACCEPT 2>/dev/null
	-sudo iptables -D FORWARD -s $(FENCE_SUB) -j DROP 2>/dev/null
	-docker network rm $(FENCE_NET) 2>/dev/null
	@echo "[fence] removed"

run:
	$(GO) run $(CMD)

repl:
	$(GO) run $(CMD) -repl

ollama:
	$(GO) run $(CMD) -backend ollama -model $(MODEL) -repl

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

check-services:
	./scripts/check-services.sh

setup-python:
	$(PYTHON) -m venv .venv
	./.venv/bin/python -m pip install --upgrade pip
	./.venv/bin/python -m pip install -r python/requirements.txt

setup-node:
	node --version
	npm install
	npx playwright install chromium

index:
	$(GO) run $(CMD) -index "$(INDEX_PATH)"

clean:
	rm -f $(BIN) $(WINDOWS_BIN) coverage.out agent_memory.db agent_memory.db-shm agent_memory.db-wal agent_rag.db agent_rag.db-shm agent_rag.db-wal
