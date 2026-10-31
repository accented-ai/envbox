package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/envbox/buildlog"
	"github.com/coder/envbox/dockerutil/dockerfake"
	"github.com/coder/envbox/xunix"
)

func TestBootstrapOutputKeepsAttachmentOpenAfterLogLimit(t *testing.T) {
	t.Parallel()
	output, writer := io.Pipe()
	var logged bytes.Buffer
	drained := make(chan error, 1)
	go func() {
		defer output.Close()
		drained <- drainBootstrapOutput(&logged, output)
	}()
	stdout := stdcopy.NewStdWriter(writer, stdcopy.Stdout)
	stderr := stdcopy.NewStdWriter(writer, stdcopy.Stderr)
	for _, message := range []struct {
		output io.Writer
		text   string
	}{
		{stdout, strings.Repeat("a", 2048)},
		{stderr, "late bootstrap diagnostic\n"},
		{stdout, strings.Repeat("b", 128<<10)},
		{stderr, "agent still running\n"},
	} {
		_, err := io.WriteString(message.output, message.text)
		require.NoError(t, err)
	}
	select {
	case err := <-drained:
		t.Fatalf("bootstrap attachment closed before process exit: %v", err)
	default:
	}
	require.NoError(t, writer.Close())
	require.NoError(t, <-drained)
	require.Contains(t, logged.String(), "late bootstrap diagnostic\n")
	require.NotContains(t, logged.String(), "agent still running")
	require.True(t, strings.HasPrefix(logged.String(), strings.Repeat("a", 2048)))
	require.Equal(t, 1, strings.Count(logged.String(), "Bootstrap output truncated"))
	require.Less(t, logged.Len(), (64<<10)+256)
}

func TestBootstrapExitReporting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                         string
		exitCode                     int
		running, stopping, malformed bool
		inspectErr                   error
		wantFailure                  bool
	}{
		{name: "NaturalSuccess"},
		{name: "NaturalFailure", exitCode: 7, wantFailure: true},
		{name: "StillRunning", exitCode: 7, running: true},
		{name: "IntentionalShutdown", exitCode: 7, stopping: true},
		{name: "InspectionCanceled", exitCode: 7, inspectErr: context.Canceled},
		{name: "AttachmentDecodeFailure", exitCode: 7, malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader, writer := io.Pipe()
			var logged bytes.Buffer
			client := &dockerfake.MockClient{ContainerExecInspectFn: func(context.Context, string) (container.ExecInspect, error) {
				return container.ExecInspect{Running: test.running, ExitCode: test.exitCode}, test.inspectErr
			}}
			output := watchBootstrapOutput(context.Background(), slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}), buildlog.JSONLogger{Encoder: json.NewEncoder(&logged)}, client, "bootstrap", reader, func() { _ = reader.Close() })
			output.stopping.Store(test.stopping)
			if test.malformed {
				_, err := io.WriteString(writer, "invalid mux header")
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			select {
			case <-output.done:
			case <-time.After(time.Second):
				t.Fatal("bootstrap output did not finish")
			}
			require.Equal(t, test.wantFailure, strings.Contains(logged.String(), "Workspace bootstrap exited with code"))
		})
	}
}

func TestBootstrapLogShutdownCancelsBlockedTransport(t *testing.T) {
	t.Parallel()
	for _, blockSend := range []bool{false, true} {
		t.Run(fmt.Sprintf("BlockedSend=%t", blockSend), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &blockedBootstrapLogClient{ctx: ctx, blockSend: blockSend, started: make(chan struct{})}
			var attachmentClosed atomic.Bool
			output := &bootstrapOutput{done: make(chan struct{}), close: func() { attachmentClosed.Store(true) }}
			go func() {
				_ = client.Send(codersdk.LogLevelInfo, "bootstrap output")
				close(output.done)
			}()
			<-client.started
			finished := make(chan struct{})
			go func() {
				closeBootstrapLogs(output, client, cancel, time.Now().Add(10*time.Millisecond))
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("logging shutdown exceeded its deadline")
			}
			require.True(t, attachmentClosed.Load())
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.EqualValues(t, 1, client.closed.Load())
			require.False(t, client.closedWhileSending.Load())
		})
	}
}

func TestBootstrapLogShutdownCancelsInspection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, coderCancel := context.WithCancel(ctx)
	defer coderCancel()
	started := make(chan struct{})
	client := &dockerfake.MockClient{ContainerExecInspectFn: func(ctx context.Context, _ string) (container.ExecInspect, error) {
		close(started)
		<-ctx.Done()
		return container.ExecInspect{}, ctx.Err()
	}}
	output := watchBootstrapOutput(ctx, slogtest.Make(t, nil), buildlog.NopLogger{}, client, "bootstrap", strings.NewReader(""), func() {})
	<-started
	finished := make(chan struct{})
	go func() {
		closeBootstrapLogs(output, nil, coderCancel, time.Now().Add(10*time.Millisecond))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the pending bootstrap inspection")
	}
}

type blockedBootstrapLogClient struct {
	ctx                         context.Context
	blockSend                   bool
	started                     chan struct{}
	sending, closedWhileSending atomic.Bool
	closed                      atomic.Int32
}

func (c *blockedBootstrapLogClient) Send(codersdk.LogLevel, string) error {
	c.sending.Store(true)
	defer c.sending.Store(false)
	close(c.started)
	if c.blockSend {
		<-c.ctx.Done()
		return c.ctx.Err()
	}
	return nil
}

func (c *blockedBootstrapLogClient) Close() error {
	c.closed.Add(1)
	c.closedWhileSending.Store(c.sending.Load())
	if !c.blockSend {
		<-c.ctx.Done()
	}
	return nil
}

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
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "agent-file"), nil, 0o600))

	require.NoError(t, pruneBootstrapDirs(context.Background(), assetsDir, now))

	_, err := os.Stat(filepath.Join(assetsDir, "agent-stale"))
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, name := range []string{"agent-current", "agent-previous", "unrelated", "agent-file"} {
		_, err := os.Stat(filepath.Join(assetsDir, name))
		require.NoError(t, err)
	}
}
