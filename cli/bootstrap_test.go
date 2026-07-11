package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/envbox/xunix"
)

func TestHostPathForInnerPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		innerPath string
		mounts    []xunix.Mount
		want      string
		wantOK    bool
	}{
		{
			name:      "MappedHome",
			innerPath: "/home/coder/.coder",
			mounts: []xunix.Mount{
				{Source: "/mnt/workspace", Mountpoint: "/home/coder"},
			},
			want:   "/mnt/workspace/.coder",
			wantOK: true,
		},
		{
			name:      "LongestMountpoint",
			innerPath: "/home/coder/.coder",
			mounts: []xunix.Mount{
				{Source: "/mnt/root", Mountpoint: "/"},
				{Source: "/mnt/workspace", Mountpoint: "/home/coder"},
			},
			want:   "/mnt/workspace/.coder",
			wantOK: true,
		},
		{
			name:      "ReadOnly",
			innerPath: "/home/coder/.coder",
			mounts: []xunix.Mount{
				{Source: "/mnt/workspace", Mountpoint: "/home/coder", ReadOnly: true},
			},
		},
		{
			name:      "Unmapped",
			innerPath: "/home/coder/.coder",
			mounts: []xunix.Mount{
				{Source: "/mnt/data", Mountpoint: "/data"},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := hostPathForInnerPath(tt.innerPath, tt.mounts)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestPruneBootstrapDirs(t *testing.T) {
	t.Parallel()

	assetsDir := t.TempDir()
	now := time.Now()
	createDir := func(name string, age time.Duration) {
		path := filepath.Join(assetsDir, name)
		require.NoError(t, os.Mkdir(path, 0o755))
		modified := now.Add(-age)
		require.NoError(t, os.Chtimes(path, modified, modified))
	}

	createDir("agent-current", time.Hour)
	createDir("agent-previous", 2*time.Hour)
	createDir("agent-stale", 2*staleBootstrapDirAge)
	createDir("unrelated", 2*staleBootstrapDirAge)
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "agent-file"), nil, 0o644))

	require.NoError(t, pruneBootstrapDirs(context.Background(), assetsDir, now))

	_, err := os.Stat(filepath.Join(assetsDir, "agent-stale"))
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{"agent-current", "agent-previous", "unrelated", "agent-file"} {
		_, err := os.Stat(filepath.Join(assetsDir, name))
		require.NoError(t, err)
	}
}
