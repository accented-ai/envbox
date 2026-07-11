#!/usr/bin/env bash
set -euo pipefail

: "${SYSBOX_FS_COMMIT:?SYSBOX_FS_COMMIT must be set}"

sysbox_fs_dir="${SYSBOX_FS_DIR:-../sysbox-fs}"
sysbox_fs_repo="${SYSBOX_FS_REPO:-https://github.com/accented-ai/sysbox-fs.git}"

if [[ ! -d "$sysbox_fs_dir/.git" ]]; then
	printf '%s#%s\n' "$sysbox_fs_repo" "$SYSBOX_FS_COMMIT"
	exit 0
fi

sysbox_fs_dir="$(git -C "$sysbox_fs_dir" rev-parse --show-toplevel)"
actual_commit="$(git -C "$sysbox_fs_dir" rev-parse HEAD)"

if [[ "$actual_commit" != "$SYSBOX_FS_COMMIT" ]]; then
	printf 'sysbox-fs at %s is at %s; expected %s\n' \
		"$sysbox_fs_dir" "$actual_commit" "$SYSBOX_FS_COMMIT" >&2
	exit 1
fi

if [[ -n "$(git -C "$sysbox_fs_dir" status --porcelain)" ]]; then
	printf 'sysbox-fs at %s has uncommitted changes\n' "$sysbox_fs_dir" >&2
	exit 1
fi

printf '%s\n' "$sysbox_fs_dir"
