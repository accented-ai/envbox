#!/usr/bin/env bash
set -euo pipefail

fail() {
	echo "sysbox source context: $*" >&2
	exit 1
}

[[ "$#" -eq 3 ]] || fail "usage: $0 <name> <repository> <commit>"

name="$1"
repository="$2"
commit="$3"
[[ "$name" =~ ^[a-z0-9-]+$ ]] || fail "invalid component name: $name"
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || fail "invalid commit: $commit"

repo_root="$(git rev-parse --show-toplevel)"
cache_root="$repo_root/build/sysbox-sources"
context_dir="$cache_root/${name}-${commit}"
marker="$context_dir/.envbox-source-commit"

if [[ -f "$marker" && "$(<"$marker")" == "$commit" ]]; then
	printf '%s\n' "$context_dir"
	exit 0
fi
[[ ! -e "$context_dir" ]] || fail "invalid cached context at $context_dir; run make clean"

mkdir -p "$cache_root"
temporary_dir="$(mktemp -d "$cache_root/.${name}.XXXXXX")"
trap 'rm -rf "$temporary_dir"' EXIT

git_dir="$temporary_dir/repository"
generated_context="$temporary_dir/context"
git init --quiet "$git_dir"
git -C "$git_dir" remote add origin "$repository"
git -C "$git_dir" -c http.version=HTTP/1.1 fetch \
	--quiet \
	--depth 1 \
	origin \
	"$commit"

resolved_commit="$(git -C "$git_dir" rev-parse 'FETCH_HEAD^{commit}')"
[[ "$resolved_commit" == "$commit" ]] || fail "resolved $resolved_commit instead of $commit"

mkdir "$generated_context"
git -C "$git_dir" archive "$resolved_commit" | tar -x -C "$generated_context"
printf '%s\n' "$commit" >"$generated_context/.envbox-source-commit"
find "$generated_context" -type f -exec chmod a-w {} +

if ! mv -T "$generated_context" "$context_dir" 2>/dev/null; then
	[[ -f "$marker" && "$(<"$marker")" == "$commit" ]] || fail "failed to cache $name context"
fi

printf '%s\n' "$context_dir"
