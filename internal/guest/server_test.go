package guest

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func testGuest(t *testing.T) (context.Context, api.SailboxExecServiceClient, api.GuestAgentServiceClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	s := New(filepath.Join(t.TempDir(), "identity"))
	t.Cleanup(s.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	api.RegisterSailboxExecServiceServer(g, s)
	api.RegisterGuestAgentServiceServer(g, s)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	c, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	control := api.NewGuestAgentServiceClient(c)
	if _, err = control.Rebind(ctx, &api.RebindRequest{SailboxId: "parent"}); err != nil {
		t.Fatal(err)
	}
	return ctx, api.NewSailboxExecServiceClient(c), control
}
func recv(t *testing.T, s grpc.ServerStreamingClient[api.ExecServerFrame]) *api.ExecServerFrame {
	t.Helper()
	f, e := s.Recv()
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func result(t *testing.T, s grpc.ServerStreamingClient[api.ExecServerFrame]) (string, string, int32) {
	t.Helper()
	var out, errout strings.Builder
	for {
		f, e := s.Recv()
		if e != nil {
			t.Fatalf("stream ended without exit: %v", e)
		}
		if c := f.GetChunk(); c != nil {
			if c.Stream == api.Chunk_STREAM_STDERR {
				errout.Write(c.Data)
			} else {
				out.Write(c.Data)
			}
		}
		if e := f.GetExit(); e != nil {
			return out.String(), errout.String(), e.ExitCode
		}
	}
}
func TestStreamingStdinAndExit(t *testing.T) {
	ctx, c, _ := testGuest(t)
	s, e := c.StreamExec(ctx, &api.StartExec{SailboxId: "parent", ExecRequestId: "stream", Argv: []string{"/bin/sh", "-c", "printf ready; read line; printf '%s' \"$line\" >&2; exit 7"}, OpenStdin: true})
	if e != nil {
		t.Fatal(e)
	}
	if recv(t, s).GetStarted() == nil {
		t.Fatal("missing started")
	}
	if string(recv(t, s).GetChunk().Data) != "ready" {
		t.Fatal("output did not arrive before command finished")
	}
	if _, e = c.WriteExecStdin(ctx, &api.WriteExecStdinRequest{SailboxId: "parent", ExecRequestId: "stream", Data: []byte("hello\n"), Eof: true}); e != nil {
		t.Fatal(e)
	}
	_, stderr, code := result(t, s)
	if stderr != "hello" || code != 7 {
		t.Fatalf("stderr=%q exit=%d", stderr, code)
	}
}
func TestDetachAndRebindPreserveCommand(t *testing.T) {
	ctx, c, control := testGuest(t)
	attach, cancel := context.WithCancel(ctx)
	s, e := c.StreamExec(attach, &api.StartExec{SailboxId: "parent", ExecRequestId: "persist", Argv: []string{"/bin/sh", "-c", "memory=alive; printf ready; read line; printf '%s:%s' \"$memory\" \"$SAILBOX_ID\""}, OpenStdin: true})
	if e != nil {
		t.Fatal(e)
	}
	recv(t, s)
	f := recv(t, s)
	seq := f.GetChunk().Seq
	cancel()
	for {
		_, e = s.Recv()
		if e != nil {
			break
		}
	}
	if _, e = control.Rebind(ctx, &api.RebindRequest{SailboxId: "child", Fork: true}); e != nil {
		t.Fatal(e)
	}
	// A detach cleanup can race a new attach; retry until the old stream exits.
	var resumed grpc.ServerStreamingClient[api.ExecServerFrame]
	for {
		resumed, e = c.StreamExec(ctx, &api.StartExec{SailboxId: "child", ExecRequestId: "persist", StdoutResumeSeq: seq})
		if e == nil {
			_, e = resumed.Recv()
		}
		if e == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(e)
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = c.WriteExecStdin(ctx, &api.WriteExecStdinRequest{SailboxId: "child", ExecRequestId: "persist", Data: []byte("go\n")}); e != nil {
		t.Fatal(e)
	}
	out, _, code := result(t, resumed)
	if out != "alive:parent" || code != 0 {
		t.Fatalf("lost process memory/environment: %q %d", out, code)
	}
	fresh, e := c.StreamExec(ctx, &api.StartExec{SailboxId: "child", ExecRequestId: "fresh", Argv: []string{"/bin/sh", "-c", "printf '%s' \"$SAILBOX_ID\""}})
	if e != nil {
		t.Fatal(e)
	}
	out, _, code = result(t, fresh)
	if out != "child" || code != 0 {
		t.Fatalf("new identity: %q %d", out, code)
	}
}
func TestPTYResizeAndCancel(t *testing.T) {
	ctx, c, _ := testGuest(t)
	s, e := c.StreamExec(ctx, &api.StartExec{SailboxId: "parent", ExecRequestId: "tty", Argv: []string{"/bin/sh", "-c", "test -t 0 || exit 42; printf ready; read line; stty size; sleep 30"}, Pty: true, OpenStdin: true, TermCols: 80, TermRows: 24})
	if e != nil {
		t.Fatal(e)
	}
	recv(t, s)
	recv(t, s)
	if _, e = c.ResizeExec(ctx, &api.ResizeExecRequest{SailboxId: "parent", ExecRequestId: "tty", TermCols: 100, TermRows: 40}); e != nil {
		t.Fatal(e)
	}
	if _, e = c.WriteExecStdin(ctx, &api.WriteExecStdinRequest{SailboxId: "parent", ExecRequestId: "tty", Data: []byte("\n")}); e != nil {
		t.Fatal(e)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "40 100") {
		f := recv(t, s)
		if f.GetExit() != nil {
			t.Fatal("PTY command failed")
		}
		if chunk := f.GetChunk(); chunk != nil {
			output.Write(chunk.Data)
		}
	}
	if _, e = c.CancelExec(ctx, &api.CancelExecRequest{SailboxId: "parent", ExecRequestId: "tty"}); e != nil {
		t.Fatal(e)
	}
	_, _, code := result(t, s)
	if code == 0 {
		t.Fatal("cancel should signal command")
	}
	if _, e = s.Recv(); e != io.EOF {
		t.Fatalf("stream not terminal: %v", e)
	}
}
