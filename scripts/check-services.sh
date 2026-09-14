#!/usr/bin/env bash
set -euo pipefail

llama_url="${LLAMA_SERVER:-http://localhost:8080}"
ollama_url="${OLLAMA_HOST:-http://localhost:11434}"
failed=0

check_service() {
	local name="$1"
	local url="$2"

	if curl --fail --silent --show-error --max-time 5 "$url" >/dev/null; then
		printf 'OK: %s is available at %s\n' "$name" "$url"
	else
		printf 'ERROR: %s is unavailable at %s\n' "$name" "$url" >&2
		failed=1
	fi
}

check_service "llama.cpp" "$llama_url/health"
check_service "Ollama" "$ollama_url/api/tags"

exit "$failed"
