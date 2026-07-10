package cli

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/envbox/dockerutil/dockerfake"
)

func TestShutdownInnerContainer(t *testing.T) {
	t.Parallel()

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()

		var stopped bool
		client := dockerfake.MockClient{
			ContainerStopFn: func(ctx context.Context, name string, options container.StopOptions) error {
				stopped = true
				requireContextDeadlineWithin(t, ctx, defaultInnerContainerStopTimeout+innerContainerStopContextSlack)
				require.Equal(t, "container-id", name)
				require.NotNil(t, options.Timeout)
				require.Equal(t, int(defaultInnerContainerStopTimeout/time.Second), *options.Timeout)
				return nil
			},
			ContainerKillFn: func(context.Context, string, string) error {
				t.Fatal("container should not be killed after clean stop")
				return nil
			},
			ContainerRemoveFn: func(context.Context, string, container.RemoveOptions) error {
				t.Fatal("container should not be force removed after clean stop")
				return nil
			},
		}

		shutdownInnerContainer(context.Background(), slogtest.Make(t, nil), client, "container-id", defaultInnerContainerStopTimeout)
		require.True(t, stopped)
	})

	t.Run("ConfiguredStopTimeout", func(t *testing.T) {
		t.Parallel()

		const configuredTimeout = 75 * time.Second

		var stopped bool
		client := dockerfake.MockClient{
			ContainerStopFn: func(ctx context.Context, name string, options container.StopOptions) error {
				stopped = true
				requireContextDeadlineWithin(t, ctx, configuredTimeout+innerContainerStopContextSlack)
				require.Equal(t, "container-id", name)
				require.NotNil(t, options.Timeout)
				require.Equal(t, 75, *options.Timeout)
				return nil
			},
			ContainerKillFn: func(context.Context, string, string) error {
				t.Fatal("container should not be killed after clean stop")
				return nil
			},
			ContainerRemoveFn: func(context.Context, string, container.RemoveOptions) error {
				t.Fatal("container should not be force removed after clean stop")
				return nil
			},
		}

		shutdownInnerContainer(context.Background(), slogtest.Make(t, nil), client, "container-id", configuredTimeout)
		require.True(t, stopped)
	})

	t.Run("KillAndRemove", func(t *testing.T) {
		t.Parallel()

		var killed, removed bool
		client := dockerfake.MockClient{
			ContainerStopFn: func(context.Context, string, container.StopOptions) error {
				return errors.New("stop failed")
			},
			ContainerKillFn: func(_ context.Context, name string, signal string) error {
				killed = true
				require.Equal(t, "container-id", name)
				require.Equal(t, "SIGKILL", signal)
				return nil
			},
			ContainerRemoveFn: func(_ context.Context, name string, options container.RemoveOptions) error {
				removed = true
				require.Equal(t, "container-id", name)
				require.True(t, options.Force)
				require.False(t, options.RemoveVolumes)
				return nil
			},
		}

		log := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})

		shutdownInnerContainer(context.Background(), log, client, "container-id", defaultInnerContainerStopTimeout)
		require.True(t, killed)
		require.True(t, removed)
	})

	t.Run("ReservesForceCleanupBudget", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(80*time.Second))
		defer cancel()

		var killed, removed bool
		client := dockerfake.MockClient{
			ContainerStopFn: func(ctx context.Context, name string, options container.StopOptions) error {
				require.Equal(t, "container-id", name)
				require.NotNil(t, options.Timeout)
				require.GreaterOrEqual(t, *options.Timeout, 54)
				require.LessOrEqual(t, *options.Timeout, 55)
				requireContextDeadlineWithin(t, ctx, 55*time.Second+innerContainerStopContextSlack)
				return errors.New("stop timed out")
			},
			ContainerKillFn: func(ctx context.Context, name string, signal string) error {
				killed = true
				require.Equal(t, "container-id", name)
				require.Equal(t, "SIGKILL", signal)
				requireContextDeadlineWithin(t, ctx, innerContainerAPICallTimeout)
				return nil
			},
			ContainerRemoveFn: func(ctx context.Context, name string, options container.RemoveOptions) error {
				removed = true
				require.Equal(t, "container-id", name)
				require.True(t, options.Force)
				requireContextDeadlineWithin(t, ctx, innerContainerAPICallTimeout)
				return nil
			},
		}

		log := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})

		shutdownInnerContainer(ctx, log, client, "container-id", 75*time.Second)
		require.True(t, killed)
		require.True(t, removed)
	})

	t.Run("SkipsStopWhenOnlyForceCleanupBudgetRemains", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(innerContainerForceCleanupBudget+innerContainerStopContextSlack))
		defer cancel()

		var killed, removed bool
		client := dockerfake.MockClient{
			ContainerStopFn: func(context.Context, string, container.StopOptions) error {
				t.Fatal("container stop should be skipped")
				return nil
			},
			ContainerKillFn: func(context.Context, string, string) error {
				killed = true
				return nil
			},
			ContainerRemoveFn: func(context.Context, string, container.RemoveOptions) error {
				removed = true
				return nil
			},
		}

		shutdownInnerContainer(ctx, slogtest.Make(t, nil), client, "container-id", defaultInnerContainerStopTimeout)
		require.True(t, killed)
		require.True(t, removed)
	})
}

func TestShutdownBootstrapExecUsesStepDeadline(t *testing.T) {
	t.Parallel()

	var inspected bool
	client := dockerfake.MockClient{
		ContainerExecInspectFn: func(ctx context.Context, execID string) (container.ExecInspect, error) {
			inspected = true
			require.Equal(t, "exec-id", execID)
			requireContextDeadlineWithin(t, ctx, bootstrapExecShutdownTimeout)
			return container.ExecInspect{}, errors.New("inspect failed")
		},
	}

	log := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})

	shutdownBootstrapExec(context.Background(), log, client, "exec-id")
	require.True(t, inspected)
}

func TestShutdownDockerCVMUsesBoundedContext(t *testing.T) {
	t.Parallel()

	var stopped bool
	client := dockerfake.MockClient{
		ContainerStopFn: func(ctx context.Context, name string, options container.StopOptions) error {
			stopped = true
			require.Equal(t, "container-id", name)
			require.NotNil(t, options.Timeout)
			require.Equal(t, int(defaultInnerContainerStopTimeout/time.Second), *options.Timeout)
			requireContextDeadlineWithin(t, ctx, defaultInnerContainerStopTimeout+innerContainerStopContextSlack)
			return nil
		},
	}

	shutdownDockerCVM(slogtest.Make(t, nil), client, dockerCVMResult{containerID: "container-id"}, defaultInnerContainerStopTimeout, dockerCVMProcesses{})
	require.True(t, stopped)
}

func TestShutdownDockerCVMReapsOuterDaemonsInOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	order := make([]string, 0, 4)
	type observation struct {
		deadline    time.Time
		gracePeriod time.Duration
	}
	observations := make(map[string]observation)
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	client := dockerfake.MockClient{
		ContainerStopFn: func(context.Context, string, container.StopOptions) error {
			record("inner")
			return nil
		},
	}
	process := func(name string) managedProcess {
		return fakeManagedProcess{stop: func(ctx context.Context, gracePeriod time.Duration) error {
			deadline, _ := ctx.Deadline()
			mu.Lock()
			observations[name] = observation{deadline: deadline, gracePeriod: gracePeriod}
			mu.Unlock()
			record(name)
			return nil
		}}
	}

	shutdownDockerCVM(
		slogtest.Make(t, nil),
		client,
		dockerCVMResult{containerID: "container-id"},
		defaultInnerContainerStopTimeout,
		dockerCVMProcesses{
			dockerd:       process("dockerd"),
			sysboxManager: process("sysbox-mgr"),
			sysboxFS:      process("sysbox-fs"),
		},
	)

	require.Len(t, order, 4)
	require.Equal(t, []string{"inner", "dockerd"}, order[:2])
	require.ElementsMatch(t, []string{"sysbox-mgr", "sysbox-fs"}, order[2:])
	for name, maxTimeout := range map[string]time.Duration{
		"dockerd":    outerDockerdShutdownTimeout,
		"sysbox-mgr": outerSysboxShutdownTimeout,
		"sysbox-fs":  outerSysboxShutdownTimeout,
	} {
		observed := observations[name]
		require.False(t, observed.deadline.IsZero())
		require.Positive(t, time.Until(observed.deadline))
		require.LessOrEqual(t, time.Until(observed.deadline), maxTimeout)
		require.Equal(t, outerDaemonGracePeriod, observed.gracePeriod)
	}
}

func TestDockerdFallbackDataRootReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                  string
		err                   error
		recoverDockerDataRoot bool
		wantReason            string
		wantOK                bool
	}{
		{
			name:       "NoSpace",
			err:        errors.New("failed to start daemon: no space left on device"),
			wantReason: "no space left on device",
			wantOK:     true,
		},
		{
			name:                  "InputOutputRecoveryDisabled",
			err:                   errors.New("failed to start daemon: chmod /var/lib/docker: input/output error"),
			recoverDockerDataRoot: false,
			wantOK:                false,
		},
		{
			name:                  "InputOutputRecoveryEnabled",
			err:                   errors.New("failed to start daemon: chmod /var/lib/docker: input/output error"),
			recoverDockerDataRoot: true,
			wantReason:            "input/output error",
			wantOK:                true,
		},
		{
			name:   "Other",
			err:    errors.New("permission denied"),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason, ok := dockerdFallbackDataRootReason(tt.err, tt.recoverDockerDataRoot)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantReason, reason)
		})
	}
}

func requireContextDeadlineWithin(t *testing.T, ctx context.Context, max time.Duration) {
	t.Helper()

	deadline, ok := ctx.Deadline()
	require.True(t, ok, "context must have a deadline")
	remaining := time.Until(deadline)
	require.Positive(t, remaining)
	require.LessOrEqual(t, remaining, max)
}

type fakeManagedProcess struct {
	stop func(context.Context, time.Duration) error
}

func (p fakeManagedProcess) Stop(ctx context.Context, gracePeriod time.Duration) error {
	return p.stop(ctx, gracePeriod)
}
