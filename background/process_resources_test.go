package background_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"
	testingexec "k8s.io/utils/exec/testing"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/envbox/background"
	"github.com/coder/envbox/xunix"
	"github.com/coder/envbox/xunix/xunixfake"
)

func TestProcessReleasesLogPipe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		startError error
	}{
		{name: "StartFailure", startError: xerrors.New("start failed")},
		{name: "CompletedWait"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := &xunixfake.FakeCmd{FakeCmd: &testingexec.FakeCmd{StartResponse: tc.startError}}
			execer := &processExecer{cmd: cmd}
			process := background.New(xunix.WithExecer(ctx, execer), slogtest.Make(t, nil), "daemon", "daemon")
			if tc.startError != nil {
				require.ErrorIs(t, process.Start(), tc.startError)
			} else {
				require.NoError(t, process.Start())
				require.NoError(t, <-process.Wait())
			}

			writeResult := make(chan error, 1)
			go func() {
				_, err := cmd.Stdout.Write([]byte("late output\n"))
				writeResult <- err
			}()
			select {
			case err := <-writeResult:
				require.ErrorIs(t, err, io.ErrClosedPipe)
			case <-time.After(time.Second):
				t.Fatal("log pipe stayed open after command completion")
			}
			require.Eventually(t, func() bool { return execer.ctx.Err() == context.Canceled }, time.Second, time.Millisecond)
		})
	}
}

type processExecer struct {
	ctx context.Context
	cmd xunix.Cmd
}

func (e *processExecer) CommandContext(ctx context.Context, _ string, _ ...string) xunix.Cmd {
	e.ctx = ctx
	return e.cmd
}
