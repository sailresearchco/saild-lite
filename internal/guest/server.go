// Package guest owns command processes. Transport disconnection detaches an
// observer; it does not terminate a command or erase its bounded output log.
package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/sailresearchco/saild-lite/api"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const maxSessions = 64
const maxOutput = 1 << 20

type Server struct {
	api.UnimplementedSailboxExecServiceServer
	api.UnimplementedGuestAgentServiceServer
	mu          sync.Mutex
	id, path    string
	sessions    map[string]*session
	SetHostname bool
}
type session struct {
	mu                 sync.Mutex
	inputMu            sync.Mutex
	req                *api.StartExec
	cmd                *exec.Cmd
	stdin              io.WriteCloser
	terminal           *os.File
	outputDone         chan struct{}
	frames             []*api.ExecServerFrame
	bytes              int
	seq                [3]int64
	notify             chan struct{}
	finished, attached bool
	exit               int32
	created            time.Time
}

func New(identityPath string) *Server {
	return &Server{path: identityPath, sessions: map[string]*session{}}
}
func (s *Server) Ping(context.Context, *api.Empty) (*api.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &api.Identity{SailboxId: s.id}, nil
}
func (s *Server) Sync(context.Context, *api.Empty) (*api.Empty, error) {
	unix.Sync()
	return &api.Empty{}, nil
}
func (s *Server) Rebind(ctx context.Context, r *api.RebindRequest) (*api.Empty, error) {
	if r.SailboxId == "" || len(r.SailboxId) > 64 || strings.ContainsAny(r.SailboxId, "/\x00\n") {
		return nil, status.Error(codes.InvalidArgument, "invalid identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.path+".tmp", []byte(r.SailboxId+"\n"), 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(s.path+".tmp", s.path); err != nil {
		return nil, err
	}
	if s.SetHostname {
		if err := setHostname(r.SailboxId); err != nil {
			return nil, err
		}
	}
	s.id = r.SailboxId
	return &api.Empty{}, nil
}
func (e *session) signal() { close(e.notify); e.notify = make(chan struct{}) }

type output struct {
	e      *session
	stream api.Chunk_Stream
}

func (o output) Write(b []byte) (int, error) {
	n := len(b)
	// Bound every frame as well as the retained aggregate.
	for len(b) > 0 {
		chunk := min(len(b), 16<<10)
		data := slices.Clone(b[:chunk])
		b = b[chunk:]
		o.e.mu.Lock()
		o.e.seq[o.stream]++
		o.e.frames = append(o.e.frames, &api.ExecServerFrame{Frame: &api.ExecServerFrame_Chunk{Chunk: &api.Chunk{Stream: o.stream, Data: data, Seq: o.e.seq[o.stream]}}})
		o.e.bytes += len(data)
		for o.e.bytes > maxOutput && len(o.e.frames) > 0 {
			o.e.bytes -= len(o.e.frames[0].GetChunk().Data)
			o.e.frames[0] = nil
			o.e.frames = o.e.frames[1:]
		}
		o.e.signal()
		o.e.mu.Unlock()
	}
	return n, nil
}
func normalize(r *api.StartExec) *api.StartExec {
	r = proto.Clone(r).(*api.StartExec)
	r.StdoutResumeSeq = 0
	r.StderrResumeSeq = 0
	r.SailboxId = ""
	return r
}
func (s *Server) getOrStart(r *api.StartExec) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.SailboxId != s.id {
		return nil, status.Error(codes.NotFound, "guest identity does not match sailbox")
	}
	if r.ExecRequestId == "" || len(r.ExecRequestId) > 128 {
		return nil, status.Error(codes.InvalidArgument, "exec_request_id required (max 128 bytes)")
	}
	if e, ok := s.sessions[r.ExecRequestId]; ok {
		if len(r.Argv) > 0 && !proto.Equal(normalize(r), normalize(e.req)) {
			return nil, status.Error(codes.AlreadyExists, "exec id belongs to a different command")
		}
		return e, nil
	}
	if len(r.Argv) == 0 {
		return nil, status.Error(codes.NotFound, "exec session not found")
	}
	if len(s.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for id, e := range s.sessions {
			e.mu.Lock()
			finished := e.finished && !e.attached
			e.mu.Unlock()
			if finished && (oldestID == "" || e.created.Before(oldest)) {
				oldestID = id
				oldest = e.created
			}
		}
		if oldestID == "" {
			return nil, status.Error(codes.ResourceExhausted, "too many active exec sessions")
		}
		delete(s.sessions, oldestID)
	}
	e := &session{req: proto.Clone(r).(*api.StartExec), notify: make(chan struct{}), created: time.Now()}
	e.cmd = exec.Command(r.Argv[0], r.Argv[1:]...)
	e.cmd.Dir = r.Cwd
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			env[k] = v
		}
	}
	for k, v := range r.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, status.Error(codes.InvalidArgument, "invalid environment")
		}
		env[k] = v
	}
	env["SAILBOX_ID"] = s.id
	env["SAILBOX_ID_FILE"] = s.path
	if r.Pty {
		env["TERM"] = r.Term
		if env["TERM"] == "" {
			env["TERM"] = "xterm-256color"
		}
	}
	for k, v := range env {
		e.cmd.Env = append(e.cmd.Env, k+"="+v)
	}
	var err error
	if r.Pty {
		e.terminal, err = pty.StartWithSize(e.cmd, winsize(r.TermCols, r.TermRows))
		e.stdin = e.terminal
		if err == nil {
			e.outputDone = make(chan struct{})
			go func() { defer close(e.outputDone); _, _ = io.Copy(output{e, api.Chunk_STREAM_STDOUT}, e.terminal) }()
		}
	} else {
		e.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		e.cmd.Stdout = output{e, api.Chunk_STREAM_STDOUT}
		e.cmd.Stderr = output{e, api.Chunk_STREAM_STDERR}
		if r.OpenStdin {
			e.stdin, err = e.cmd.StdinPipe()
		}
		if err == nil {
			err = e.cmd.Start()
		}
	}
	if err != nil {
		if e.stdin != nil {
			e.stdin.Close()
		}
		return nil, status.Errorf(codes.Internal, "start command: %v", err)
	}
	s.sessions[r.ExecRequestId] = e
	go e.wait(r.TimeoutSeconds)
	return e, nil
}
func winsize(cols, rows uint32) *pty.Winsize {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return &pty.Winsize{Cols: uint16(min(cols, 65535)), Rows: uint16(min(rows, 65535))}
}
func (e *session) kill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.finished {
		_ = syscall.Kill(-e.cmd.Process.Pid, syscall.SIGKILL)
	}
}
func (e *session) wait(timeout uint32) {
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(time.Duration(timeout)*time.Second, e.kill)
		defer timer.Stop()
	}
	err := e.cmd.Wait()
	code := int32(0)
	if err != nil {
		code = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = int32(exit.ExitCode())
			if code < 0 {
				code = 128 + int32(exit.Sys().(syscall.WaitStatus).Signal())
			}
		}
	}
	// Wait drains pipe writers; a PTY reader needs its own drain boundary.
	if e.outputDone != nil {
		select {
		case <-e.outputDone:
		case <-time.After(time.Second):
			_ = e.terminal.Close()
			<-e.outputDone
		}
	}
	e.inputMu.Lock()
	if e.stdin != nil {
		_ = e.stdin.Close()
		e.stdin = nil
	}
	e.inputMu.Unlock()
	e.mu.Lock()
	e.exit = code
	e.finished = true
	e.signal()
	e.mu.Unlock()
}
func (s *Server) StreamExec(r *api.StartExec, stream grpc.ServerStreamingServer[api.ExecServerFrame]) error {
	e, err := s.getOrStart(r)
	if err != nil {
		return err
	}
	e.mu.Lock()
	if e.attached {
		e.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "exec is already attached")
	}
	e.attached = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.attached = false; e.mu.Unlock() }()
	if err = stream.Send(&api.ExecServerFrame{Frame: &api.ExecServerFrame_Started{Started: &api.Started{}}}); err != nil {
		return err
	}
	cursors := [3]int64{0, r.StdoutResumeSeq, r.StderrResumeSeq}
	for {
		e.mu.Lock()
		var frames []*api.ExecServerFrame
		for _, f := range e.frames {
			c := f.GetChunk()
			if c.Seq > cursors[c.Stream] {
				frames = append(frames, f)
			}
		}
		done, exit, notify := e.finished, e.exit, e.notify
		e.mu.Unlock()
		for _, f := range frames {
			if err = stream.Send(f); err != nil {
				return err
			}
			c := f.GetChunk()
			cursors[c.Stream] = c.Seq
		}
		if done {
			return stream.Send(&api.ExecServerFrame{Frame: &api.ExecServerFrame_Exit{Exit: &api.Exit{ExitCode: exit}}})
		}
		select {
		case <-notify:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}
func (s *Server) lookup(id, request string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.id {
		return nil, status.Error(codes.NotFound, "guest identity mismatch")
	}
	e := s.sessions[request]
	if e == nil {
		return nil, status.Error(codes.NotFound, "exec not found")
	}
	return e, nil
}
func (s *Server) WriteExecStdin(ctx context.Context, r *api.WriteExecStdinRequest) (*api.Empty, error) {
	e, err := s.lookup(r.SailboxId, r.ExecRequestId)
	if err != nil {
		return nil, err
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.stdin == nil {
		return nil, status.Error(codes.FailedPrecondition, "stdin closed")
	}
	if len(r.Data) > 0 {
		if _, err = e.stdin.Write(r.Data); err != nil {
			return nil, err
		}
	}
	if r.Eof {
		if e.req.Pty {
			_, err = e.stdin.Write([]byte{4})
		} else {
			err = e.stdin.Close()
			e.stdin = nil
		}
	}
	return &api.Empty{}, err
}
func (s *Server) ResizeExec(ctx context.Context, r *api.ResizeExecRequest) (*api.Empty, error) {
	e, err := s.lookup(r.SailboxId, r.ExecRequestId)
	if err != nil {
		return nil, err
	}
	if e.terminal == nil {
		return nil, status.Error(codes.FailedPrecondition, "exec has no PTY")
	}
	return &api.Empty{}, pty.Setsize(e.terminal, winsize(r.TermCols, r.TermRows))
}
func (s *Server) CancelExec(ctx context.Context, r *api.CancelExecRequest) (*api.Empty, error) {
	e, err := s.lookup(r.SailboxId, r.ExecRequestId)
	if status.Code(err) == codes.NotFound {
		return &api.Empty{}, nil
	}
	if err != nil {
		return nil, err
	}
	e.kill()
	return &api.Empty{}, nil
}
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.sessions {
		e.kill()
	}
}
