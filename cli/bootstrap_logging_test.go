package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/common"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/hashicorp/yamux"
	"github.com/stretchr/testify/require"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	"github.com/coder/coder/v2/agent/proto"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/envbox/buildlog"
	"github.com/coder/envbox/cli/clitest"
	"github.com/coder/websocket"
)

type bootstrapLogServer struct {
	proto.DRPCAgentUnimplementedServer
	output chan string
}

func (s *bootstrapLogServer) BatchCreateLogs(_ context.Context, request *proto.BatchCreateLogsRequest) (*proto.BatchCreateLogsResponse, error) {
	for _, log := range request.Logs {
		s.output <- log.Output
	}
	return &proto.BatchCreateLogsResponse{}, nil
}

func TestBootstrapLogsContinueAfterStartupMarker(t *testing.T) {
	t.Parallel()

	recorder := &bootstrapLogServer{output: make(chan string, 128)}
	rpc := drpcmux.New()
	require.NoError(t, proto.DRPCRegisterAgent(rpc, recorder))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(codersdk.BuildInfoResponse{Version: "v2.33.10"})
	})
	mux.HandleFunc("/api/v2/workspaceagents/me/rpc", func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		config := yamux.DefaultConfig()
		config.LogOutput = io.Discard
		session, err := yamux.Server(websocket.NetConn(r.Context(), connection, websocket.MessageBinary), config)
		if err != nil {
			return
		}
		defer session.Close()
		_ = drpcserver.New(rpc).Serve(r.Context(), session)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx, cmd := clitest.New(t, "docker", "--image=ubuntu", "--username=root",
		"--agent-token=test-token", "--coder-url="+server.URL,
		"--no-startup-log=false", "--boostrap-script=echo hello")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	execer := clitest.Execer(ctx)
	execer.DefaultFakeCmd.FakeProcess = &os.Process{Pid: 1 << 30}
	execer.DefaultFakeCmd.WaitFn = func() error { <-ctx.Done(); return ctx.Err() }

	startupDone := make(chan struct{}, 1)
	diagnostics := make(chan string, 128)
	cmd.SetErr(startupMarkerWriter{done: startupDone, diagnostics: diagnostics})

	attachment, output := net.Pipe()
	defer output.Close()
	client := clitest.DockerClient(t, ctx)
	attach := client.ContainerExecAttachFn
	client.ContainerExecCreateFn = func(_ context.Context, _ string, config container.ExecOptions) (common.IDResponse, error) {
		if len(config.Cmd) == 2 && config.Cmd[0] == "/bin/sh" && config.Cmd[1] == "-s" {
			return common.IDResponse{ID: "bootstrap-exec"}, nil
		}
		return common.IDResponse{ID: "setup-exec"}, nil
	}
	client.ContainerExecAttachFn = func(ctx context.Context, id string, options container.ExecAttachOptions) (dockertypes.HijackedResponse, error) {
		if id == "bootstrap-exec" {
			return dockertypes.HijackedResponse{Conn: attachment, Reader: bufio.NewReader(attachment)}, nil
		}
		return attach(ctx, id, options)
	}
	client.ContainerExecInspectFn = func(_ context.Context, id string) (container.ExecInspect, error) {
		if id == "bootstrap-exec" {
			return container.ExecInspect{ExitCode: 7, Pid: 1 << 30}, nil
		}
		return container.ExecInspect{}, nil
	}
	go func() { _, _ = io.CopyN(io.Discard, output, int64(len("echo hello"))) }()
	require.NoError(t, cmd.ExecuteContext(ctx))
	select {
	case <-startupDone:
	case <-time.After(time.Second):
		t.Fatal("startup readiness marker was not emitted")
	}
	_, err := io.WriteString(stdcopy.NewStdWriter(output, stdcopy.Stderr), "delayed bootstrap diagnostic\n")
	require.NoError(t, err)
	require.NoError(t, output.Close())
	for _, expected := range []string{"delayed bootstrap diagnostic\n", "Workspace bootstrap exited with code 7."} {
		deadline := time.After(3 * time.Second)
		found := false
		var logs []string
		for !found {
			select {
			case message := <-recorder.output:
				found = strings.Contains(message, expected)
				logs = append(logs, message)
			case message := <-diagnostics:
				logs = append(logs, message)
			case <-deadline:
				t.Fatalf("Coder transport stopped before %q: %s", expected, strings.Join(logs, "\n"))
			}
		}
	}
}

type startupMarkerWriter struct {
	done        chan struct{}
	diagnostics chan string
}

func (w startupMarkerWriter) Write(p []byte) (int, error) {
	var log buildlog.JSONLog
	select {
	case w.diagnostics <- string(p):
	default:
	}
	if json.Unmarshal(p, &log) == nil && log.Type == buildlog.JSONLogTypeDone {
		select {
		case w.done <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}
