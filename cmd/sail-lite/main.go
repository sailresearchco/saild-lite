package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("command exited %d", e) }
func main() {
	if err := run(os.Args[1:]); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}
func run(args []string) error {
	flags := flag.NewFlagSet("sail-lite", flag.ContinueOnError)
	defaultAddr := os.Getenv("SAILD_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:50051"
	}
	addr := flags.String("addr", defaultAddr, "saild gRPC address (normally SSH forwarded)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return fmt.Errorf("usage: sail-lite [--addr HOST:PORT] create|list|stop|fork|checkpoint|restore|exec|shell|tui")
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	if args[0] == "tui" {
		return tui(conn, *addr)
	}
	return command(conn, args)
}
func command(conn *grpc.ClientConn, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := api.NewFirecrackerControlServiceClient(conn)
	printVM := func(v *api.VM, err error) error {
		if err != nil {
			return err
		}
		fmt.Println(v.SailboxId)
		return nil
	}
	switch args[0] {
	case "create":
		f := flag.NewFlagSet("create", flag.ContinueOnError)
		id := f.String("id", newID("sb"), "sailbox ID")
		cpu := f.Uint("cpu", 1, "vCPUs")
		mem := f.Uint("memory", 512, "RAM MiB")
		image := f.String("image", "default", "image ID")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected create arguments")
		}
		v, e := c.StartFirecrackerVM(ctx, &api.StartFirecrackerVMRequest{SailboxId: *id, VcpuCount: uint32(*cpu), MemSizeMib: uint32(*mem), ImageId: *image})
		return printVM(v, e)
	case "list":
		v, e := c.GetWorkerState(ctx, &api.Empty{})
		if e != nil {
			return e
		}
		if len(args) > 1 && args[1] == "--json" {
			return json.NewEncoder(os.Stdout).Encode(v)
		}
		fmt.Printf("CPU %d/%d   RAM %d/%d MiB\n", v.ReservedVcpus, v.TotalVcpus, v.ReservedMemoryMib, v.TotalMemoryMib)
		fmt.Printf("%-25s %-12s %5s %8s\n", "SAILBOX", "STATE", "CPU", "RAM MiB")
		for _, vm := range v.Vms {
			fmt.Printf("%-25s %-12s %5d %8d\n", vm.SailboxId, vm.State, vm.VcpuCount, vm.MemSizeMib)
		}
		return nil
	case "stop":
		if len(args) != 2 {
			return errors.New("usage: sail-lite stop SAILBOX_ID")
		}
		v, e := c.GetWorkerState(ctx, &api.Empty{})
		if e != nil {
			return e
		}
		for _, vm := range v.Vms {
			if vm.SailboxId == args[1] {
				_, e = c.StopFirecrackerVM(ctx, &api.StopFirecrackerVMRequest{VmId: vm.VmId})
				return e
			}
		}
		return fmt.Errorf("sailbox %q not found", args[1])
	case "fork", "restore":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: sail-lite %s SOURCE [NEW_SAILBOX_ID]", args[0])
		}
		id := newID("sb")
		if len(args) == 3 {
			id = args[2]
		}
		if args[0] == "fork" {
			v, e := c.ForkFirecrackerVM(ctx, &api.ForkFirecrackerVMRequest{SailboxId: args[1], ChildSailboxId: id})
			return printVM(v, e)
		}
		v, e := c.RestoreFirecrackerVMFromCheckpoint(ctx, &api.RestoreFirecrackerVMFromCheckpointRequest{SailboxId: id, CheckpointId: args[1]})
		return printVM(v, e)
	case "checkpoint":
		if len(args) != 2 {
			return errors.New("usage: sail-lite checkpoint SAILBOX_ID")
		}
		v, e := c.CheckpointFirecrackerVM(ctx, &api.CheckpointFirecrackerVMRequest{SailboxId: args[1]})
		if e != nil {
			return e
		}
		fmt.Println(v.CheckpointId)
		return nil
	case "exec", "shell":
		if len(args) < 2 {
			return errors.New("usage: sail-lite exec ID -- COMMAND [ARGS...] | sail-lite shell ID")
		}
		argv := args[2:]
		isShell := args[0] == "shell"
		if isShell {
			argv = []string{"/bin/bash", "-l"}
		} else {
			if len(argv) > 0 && argv[0] == "--" {
				argv = argv[1:]
			}
			if len(argv) == 0 {
				return errors.New("exec requires a command")
			}
		}
		return execute(conn, args[1], argv, isShell)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func execute(conn *grpc.ClientConn, id string, argv []string, isShell bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &api.StartExec{SailboxId: id, ExecRequestId: newID("exec"), Argv: argv, Pty: isShell, OpenStdin: isShell}
	if isShell {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("shell needs a terminal")
		}
		cols, rows, _ := term.GetSize(int(os.Stdin.Fd()))
		req.TermCols = uint32(cols)
		req.TermRows = uint32(rows)
		req.Term = os.Getenv("TERM")
		old, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	c := api.NewSailboxExecServiceClient(conn)
	stream, err := c.StreamExec(ctx, req)
	if err != nil {
		return err
	}
	// Only write stdin after Started confirms that the guest owns the session.
	frame, err := stream.Recv()
	if err != nil {
		return err
	}
	if frame.GetStarted() == nil {
		return errors.New("exec stream did not start")
	}
	if isShell {
		inputDone := make(chan struct{})
		go func() { defer close(inputDone); pumpInput(ctx, c, req) }()
		defer func() { cancel(); <-inputDone }()
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-resize:
					cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
					if err == nil {
						call, done := context.WithTimeout(ctx, 2*time.Second)
						_, _ = c.ResizeExec(call, &api.ResizeExecRequest{SailboxId: id, ExecRequestId: req.ExecRequestId, TermCols: uint32(cols), TermRows: uint32(rows)})
						done()
					}
				}
			}
		}()
	}
	return drain(stream, os.Stdout, os.Stderr)
}
func drain(stream grpc.ServerStreamingClient[api.ExecServerFrame], stdout, stderr io.Writer) error {
	for {
		f, err := stream.Recv()
		if err == io.EOF {
			return errors.New("exec detached before exit; process may still be running")
		}
		if err != nil {
			return err
		}
		if c := f.GetChunk(); c != nil {
			w := stdout
			if c.Stream == api.Chunk_STREAM_STDERR {
				w = stderr
			}
			if _, err = w.Write(c.Data); err != nil {
				return err
			}
		}
		if e := f.GetExit(); e != nil {
			if e.ExitCode != 0 {
				return exitCode(e.ExitCode)
			}
			return nil
		}
	}
}
func pumpInput(ctx context.Context, c api.SailboxExecServiceClient, r *api.StartExec) {
	b := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		n, err = os.Stdin.Read(b)
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, sendErr := c.WriteExecStdin(call, &api.WriteExecStdinRequest{SailboxId: r.SailboxId, ExecRequestId: r.ExecRequestId, Data: b[:n], Eof: err != nil})
		cancel()
		if err != nil || sendErr != nil {
			return
		}
	}
}
func tui(conn *grpc.ClientConn, addr string) error {
	fmt.Println("saild-lite — create | list | fork ID | stop ID | shell ID | checkpoint ID | restore CP | quit")
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("sail-lite> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		args := strings.Fields(line)
		if len(args) == 0 {
			continue
		}
		if args[0] == "quit" || args[0] == "exit" {
			return nil
		}
		if err := command(conn, args); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}
