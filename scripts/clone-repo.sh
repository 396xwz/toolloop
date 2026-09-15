#!/usr/bin/env bash
set -euo pipefail

usage() {
	printf 'Usage: scripts/clone-repo.sh <repository-url> [directory-name]\n' >&2
}

die() {
	printf 'Error: %s\n' "$1" >&2
	exit 1
}

trim_trailing_slashes() {
	local value="$1"
	while [[ "$value" == */ ]]; do
		value="${value%/}"
	done
	printf '%s\n' "$value"
}

derive_directory_name() {
	local repo_url="$1"
	local trimmed
	local name

	trimmed="$(trim_trailing_slashes "$repo_url")"
	name="${trimmed##*/}"
	name="${name##*:}"
	name="${name%.git}"
	printf '%s\n' "$name"
}

validate_directory_name() {
	local name="$1"

	if [[ -z "$name" ]]; then
		die 'directory name must not be empty'
	fi
	if [[ "$name" == "." || "$name" == ".." ]]; then
		die "directory name '$name' is not allowed"
	fi
	if [[ "$name" == *"/"* || "$name" == *"\\"* ]]; then
		die "directory name '$name' must be a single basename"
	fi
	if [[ ! "$name" =~ ^[A-Za-z0-9._-]+$ ]]; then
		die "directory name '$name' contains unsafe characters; allowed: letters, digits, dot, underscore, hyphen"
	fi
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
	usage
	exit 1
fi

if ! command -v git >/dev/null 2>&1; then
	die 'git is required but was not found on PATH'
fi

repository_url="$1"
directory_name="${2:-$(derive_directory_name "$repository_url")}"

validate_directory_name "$directory_name"

target_path="/tmp/$directory_name"
created_target=0

cleanup_target_path() {
	local status=$?

	if [[ $created_target -eq 1 && $status -ne 0 ]]; then
		rmdir -- "$target_path" 2>/dev/null || true
	fi

	return "$status"
}

trap cleanup_target_path EXIT

if ! mkdir -m 0700 -- "$target_path" 2>/dev/null; then
	die "refusing to reuse existing path '$target_path'"
fi
created_target=1

cd -- "$target_path"
git clone --filter=blob:none --no-tags -- "$repository_url" .

trap - EXIT

printf '%s\n' "$target_path"
