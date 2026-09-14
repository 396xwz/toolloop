BIN := toolloop
WINDOWS_BIN ?= toolloop.exe
CMD := ./cmd/toolloop
GO ?= go
PYTHON ?= python3
MODEL ?= qwen2.5:14b
INDEX_PATH ?= .

.DEFAULT_GOAL := help

.PHONY: help build build-windows run repl ollama test vet fmt tidy check-services setup-python index clean

help:
	@printf '%s\n' \
		'Toolloop targets:' \
		'  make build                 Build ./toolloop' \
		'  make build-windows         Cross-build a Windows amd64 binary' \
		'  make run                   Run the default llama.cpp backend' \
		'  make repl                  Start the default llama.cpp REPL' \
		'  make ollama                Start an Ollama REPL (MODEL=qwen2.5:14b)' \
		'  make test                  Run Go tests' \
		'  make vet                   Run go vet' \
		'  make fmt                   Format Go source files' \
		'  make tidy                  Tidy Go module dependencies' \
		'  make check-services        Check llama.cpp and Ollama endpoints' \
		'  make setup-python          Create .venv and install requirements' \
		'  make index INDEX_PATH=dir  Index a directory into RAG' \
		'  make clean                 Remove generated local artifacts'

build:
	$(GO) build -o $(BIN) $(CMD)

build-windows:
	GOOS=windows GOARCH=amd64 $(GO) build -o $(WINDOWS_BIN) $(CMD)

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

index:
	$(GO) run $(CMD) -index "$(INDEX_PATH)"

clean:
	rm -f $(BIN) $(WINDOWS_BIN) coverage.out agent_memory.db agent_memory.db-shm agent_memory.db-wal agent_rag.db agent_rag.db-shm agent_rag.db-wal
