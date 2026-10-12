//go:build smoke

package tests

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMemoryFork runs only against an explicitly selected, empty real host.
// The secret exists only in Python's RAM; a disk clone and cold boot cannot
// reproduce it or its live PID/listening socket.
func TestMemoryFork(t *testing.T) {
	addr := os.Getenv("SAILD_ADDR")
	if addr == "" {
		t.Fatal("SAILD_ADDR required: choose a disposable host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	control := api.NewFirecrackerControlServiceClient(conn)
	exec := api.NewSailboxExecServiceClient(conn)
	state, err := control.GetWorkerState(ctx, &api.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Vms) > 0 {
		t.Fatal("smoke requires an empty host")
	}
	var vms []*api.VM
	defer func() {
		for _, vm := range vms {
			stop, done := context.WithTimeout(context.Background(), 15*time.Second)
			_, err := control.StopFirecrackerVM(stop, &api.StopFirecrackerVMRequest{VmId: vm.VmId})
			done()
			if err != nil {
				t.Error(err)
			}
		}
	}()
	parent, err := control.StartFirecrackerVM(ctx, &api.StartFirecrackerVMRequest{SailboxId: "smoke-parent", VcpuCount: 1, MemSizeMib: 512, ImageId: "default"})
	if err != nil {
		t.Fatal(err)
	}
	vms = append(vms, parent)
	sequence := 0
	command := func(id string, argv ...string) string {
		t.Helper()
		sequence++
		s, err := exec.StreamExec(ctx, &api.StartExec{SailboxId: id, ExecRequestId: fmt.Sprintf("exec-%d", sequence), Argv: argv})
		if err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for {
			f, err := s.Recv()
			if err != nil {
				t.Fatalf("exec %v: %v: %s", argv, err, output.String())
			}
			if c := f.GetChunk(); c != nil {
				output.Write(c.Data)
			}
			if e := f.GetExit(); e != nil {
				if e.ExitCode != 0 {
					t.Fatalf("exec %v exited %d: %s", argv, e.ExitCode, output.String())
				}
				return output.String()
			}
		}
	}
	command(parent.SailboxId, "/bin/sh", "-c", "printf parent > /root/independent; curl --fail --silent --max-time 20 https://example.com > /dev/null")
	program := `import os,socket
secret=os.urandom(32).hex()
server=socket.socket()
server.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
server.bind(('127.0.0.1',9011))
server.listen()
print('ready',flush=True)
while True:
    c,_=server.accept()
    c.sendall((str(os.getpid())+':'+secret).encode())
    c.close()
`
	stream, err := exec.StreamExec(ctx, &api.StartExec{SailboxId: parent.SailboxId, ExecRequestId: "memory-server", Argv: []string{"python3", "-u", "-c", program}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		f, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if c := f.GetChunk(); c != nil && strings.Contains(string(c.Data), "ready") {
			break
		}
		if f.GetExit() != nil {
			t.Fatal("memory process exited")
		}
	}
	readMemory := func(id string) string {
		return command(id, "python3", "-c", "import socket; s=socket.create_connection(('127.0.0.1',9011)); print(s.recv(256).decode())")
	}
	before := readMemory(parent.SailboxId)
	child, err := control.ForkFirecrackerVM(ctx, &api.ForkFirecrackerVMRequest{SailboxId: parent.SailboxId, ChildSailboxId: "smoke-child"})
	if err != nil {
		t.Fatal(err)
	}
	vms = append(vms, child)
	if got := readMemory(child.SailboxId); got != before {
		t.Fatalf("child lost RAM/PID: before=%q after=%q", before, got)
	}
	if got := readMemory(parent.SailboxId); got != before {
		t.Fatalf("parent changed during fork: %q", got)
	}
	identity := command(child.SailboxId, "/bin/sh", "-c", "printf '%s:' \"$SAILBOX_ID\"; cat /run/sailbox-id")
	if identity != "smoke-child:smoke-child\n" {
		t.Fatalf("child identity: %q", identity)
	}
	command(child.SailboxId, "/bin/sh", "-c", "printf child > /root/independent")
	if got := command(parent.SailboxId, "cat", "/root/independent"); got != "parent" {
		t.Fatalf("child modified parent disk: %q", got)
	}
	if got := command(child.SailboxId, "cat", "/root/independent"); got != "child" {
		t.Fatalf("child disk: %q", got)
	}
	checkpoint, err := control.CheckpointFirecrackerVM(ctx, &api.CheckpointFirecrackerVMRequest{SailboxId: child.SailboxId})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.StopFirecrackerVM(ctx, &api.StopFirecrackerVMRequest{VmId: child.VmId}); err != nil {
		t.Fatal(err)
	}
	restored, err := control.RestoreFirecrackerVMFromCheckpoint(ctx, &api.RestoreFirecrackerVMFromCheckpointRequest{SailboxId: "smoke-restored", CheckpointId: checkpoint.CheckpointId})
	if err != nil {
		t.Fatal(err)
	}
	vms = append(vms, restored)
	if got := readMemory(restored.SailboxId); got != before {
		t.Fatalf("checkpoint lost RAM: %q", got)
	}
	if got := command(restored.SailboxId, "cat", "/root/independent"); got != "child" {
		t.Fatalf("checkpoint disk mismatch: %q", got)
	}
	for _, vm := range vms {
		if _, err = control.StopFirecrackerVM(ctx, &api.StopFirecrackerVMRequest{VmId: vm.VmId}); err != nil {
			t.Fatal(err)
		}
	}
	state, err = control.GetWorkerState(ctx, &api.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Vms) != 0 || state.ReservedVcpus != 0 || state.ReservedMemoryMib != 0 {
		t.Fatalf("accounting not reset: %v", state)
	}
	// The parent's live exec stream ends when its VM is stopped.
	for {
		_, err = stream.Recv()
		if err != nil {
			if err != io.EOF {
				t.Logf("VM teardown closed exec: %v", err)
			}
			break
		}
	}
	t.Log("verified live process PID + RAM, parent continuation, child identity, disk independence, checkpoint restore, streaming exec and zero accounting")
}
