package cli

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/docker/docker/pkg/stdcopy"

	"cdr.dev/slog/v3"

	"github.com/coder/envbox/buildlog"
	"github.com/coder/envbox/dockerutil"
)

const (
	bootstrapLogLimit           = 64 << 10
	bootstrapLogShutdownTimeout = 5 * time.Second
)

type bootstrapOutput struct {
	close    func()
	done     chan struct{}
	stopping atomic.Bool
}

func watchBootstrapOutput(ctx context.Context, log slog.Logger, blog buildlog.Logger, client dockerutil.Client, execID string, reader io.Reader, closeOutput func()) *bootstrapOutput {
	ctx, cancel := context.WithCancel(ctx)
	output := &bootstrapOutput{done: make(chan struct{}), close: func() {
		cancel()
		closeOutput()
	}}
	go func() {
		select {
		case <-ctx.Done():
			output.close()
		case <-output.done:
		}
	}()
	go func() {
		defer close(output.done)
		defer output.close()
		if err := drainBootstrapOutput(blog, reader); err != nil {
			if ctx.Err() == nil && !output.stopping.Load() {
				log.Error(ctx, "copy bootstrap output", slog.Error(err))
			}
			return
		}
		if ctx.Err() != nil || output.stopping.Load() {
			return
		}
		inspectCtx, cancel := context.WithTimeout(ctx, bootstrapLogShutdownTimeout)
		defer cancel()
		inspect, err := client.ContainerExecInspect(inspectCtx, execID)
		if err != nil {
			log.Debug(ctx, "inspect completed bootstrap", slog.Error(err))
			return
		}
		if inspectCtx.Err() == nil && !output.stopping.Load() && !inspect.Running && inspect.ExitCode != 0 {
			blog.Errorf("Workspace bootstrap exited with code %d.", inspect.ExitCode)
		}
	}()
	return output
}

func closeBootstrapLogs(output *bootstrapOutput, client buildlog.CoderClient, cancel context.CancelFunc, deadline time.Time) {
	// Cancel SDK Send/flush before joining if the transport cannot drain.
	timer := time.AfterFunc(time.Until(deadline), cancel)
	defer timer.Stop()
	defer cancel()
	if output != nil {
		output.close()
		<-output.done
	}
	if client != nil {
		_ = client.Close()
	}
}

func drainBootstrapOutput(log io.Writer, output io.Reader) error {
	limited := &bootstrapLogWriter{log: log, remaining: bootstrapLogLimit}
	_, err := stdcopy.StdCopy(limited, limited, output)
	// An attachment must keep draining even after logging or decoding fails.
	_, drainErr := io.Copy(io.Discard, output)
	if err != nil {
		return err
	}
	return drainErr
}

type bootstrapLogWriter struct {
	log       io.Writer
	remaining int
	truncated bool
}

func (w *bootstrapLogWriter) Write(p []byte) (int, error) {
	forward := min(len(p), w.remaining)
	if forward > 0 {
		if _, err := w.log.Write(p[:forward]); err != nil {
			return 0, err
		}
		w.remaining -= forward
	}
	if forward < len(p) && !w.truncated {
		w.truncated = true
		if _, err := io.WriteString(w.log, "\nBootstrap output truncated after 64 KiB; remaining output is drained.\n"); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
