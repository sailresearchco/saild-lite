// Package host owns VM identity, admission, lifecycle and the exec relay. The
// runtime owns hypervisor processes and opaque checkpoint artifacts.
package host

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Shape struct {
	CPUs, Memory uint32
	Image        string
}
type Instance interface {
	Stop(context.Context) error
	Checkpoint(context.Context, string) error
	Guest(context.Context) (*grpc.ClientConn, error)
	Done() <-chan struct{}
}
type Runtime interface {
	Start(context.Context, string, Shape) (Instance, error)
	Restore(context.Context, string, string, Shape) (Instance, error)
	DeleteCheckpoint(string) error
}
type entry struct {
	mu       sync.Mutex
	vm       *api.VM
	instance Instance
}
type checkpoint struct {
	shape  Shape
	source string
}

type Server struct {
	api.UnimplementedFirecrackerControlServiceServer
	api.UnimplementedSailboxExecServiceServer
	mu                                sync.Mutex
	vms                               map[string]*entry
	checkpoints                       map[string]checkpoint
	runtime                           Runtime
	cpus, memory, usedCPU, usedMemory uint32
	closing                           bool
}

func New(r Runtime, cpus, memory uint32) *Server {
	return &Server{runtime: r, cpus: cpus, memory: memory, vms: map[string]*entry{}, checkpoints: map[string]checkpoint{}}
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func NewID(prefix string) string { return fmt.Sprintf("%s-%x", prefix, randomBytes()) }
func randomBytes() []byte {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// reserve is the only allocation writer. Pending starts consume capacity too.
func (s *Server) reserve(id string, shape Shape) (*entry, error) {
	if !idPattern.MatchString(id) {
		return nil, status.Error(codes.InvalidArgument, "invalid sailbox_id")
	}
	if shape.CPUs == 0 || shape.CPUs > 32 || shape.Memory < 128 || !idPattern.MatchString(shape.Image) {
		return nil, status.Error(codes.InvalidArgument, "require 1..32 CPUs, >=128 MiB and a valid image_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, status.Error(codes.Unavailable, "host stopping")
	}
	if _, ok := s.vms[id]; ok {
		return nil, status.Error(codes.AlreadyExists, "sailbox exists")
	}
	if shape.CPUs > s.cpus-s.usedCPU || shape.Memory > s.memory-s.usedMemory {
		return nil, status.Error(codes.ResourceExhausted, "host capacity exhausted")
	}
	e := &entry{vm: &api.VM{SailboxId: id, VmId: NewID("vm"), VcpuCount: shape.CPUs, MemSizeMib: shape.Memory, ImageId: shape.Image, State: "starting"}}
	// Caller owns the new lifecycle lock until start/restore is resolved.
	e.mu.Lock()
	s.vms[id] = e
	s.usedCPU += shape.CPUs
	s.usedMemory += shape.Memory
	return e, nil
}
func (s *Server) release(e *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vms[e.vm.SailboxId] == e {
		delete(s.vms, e.vm.SailboxId)
		s.usedCPU -= e.vm.VcpuCount
		s.usedMemory -= e.vm.MemSizeMib
	}
}
func (s *Server) lookup(id string) (*entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.vms[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "sailbox not found")
	}
	return e, nil
}
func (s *Server) activate(ctx context.Context, e *entry, inst Instance, err error, fork bool) (*api.VM, error) {
	defer e.mu.Unlock()
	if err == nil {
		var conn *grpc.ClientConn
		conn, err = inst.Guest(ctx)
		if err == nil {
			_, err = api.NewGuestAgentServiceClient(conn).Rebind(ctx, &api.RebindRequest{SailboxId: e.vm.SailboxId, Fork: fork})
			conn.Close()
		}
	}
	if err != nil {
		if inst != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			stopErr := inst.Stop(stopCtx)
			cancel()
			if stopErr != nil {
				e.instance = inst
				s.mu.Lock()
				e.vm.State = "stop_failed"
				s.mu.Unlock()
				return nil, status.Errorf(codes.Internal, "start: %v; cleanup: %v", err, stopErr)
			}
		}
		s.release(e)
		return nil, status.Errorf(codes.Internal, "start/restore: %v", err)
	}
	e.instance = inst
	s.mu.Lock()
	e.vm.State = "running"
	result := proto.Clone(e.vm).(*api.VM)
	s.mu.Unlock()
	go func() { <-inst.Done(); e.mu.Lock(); defer e.mu.Unlock(); s.release(e) }()
	return result, nil
}
func (s *Server) StartFirecrackerVM(ctx context.Context, r *api.StartFirecrackerVMRequest) (*api.VM, error) {
	shape := Shape{r.VcpuCount, r.MemSizeMib, r.ImageId}
	e, err := s.reserve(r.SailboxId, shape)
	if err != nil {
		return nil, err
	}
	inst, err := s.runtime.Start(ctx, e.vm.VmId, shape)
	return s.activate(ctx, e, inst, err, false)
}
func (s *Server) StopFirecrackerVM(ctx context.Context, r *api.StopFirecrackerVMRequest) (*api.Empty, error) {
	s.mu.Lock()
	var e *entry
	for _, v := range s.vms {
		if v.vm.VmId == r.VmId {
			e = v
			break
		}
	}
	s.mu.Unlock()
	if e == nil {
		return &api.Empty{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.instance != nil {
		if err := e.instance.Stop(ctx); err != nil {
			return nil, status.Errorf(codes.Internal, "stop: %v", err)
		}
	}
	s.release(e)
	return &api.Empty{}, nil
}
func (s *Server) capture(ctx context.Context, id string) (string, checkpoint, error) {
	e, err := s.lookup(id)
	if err != nil {
		return "", checkpoint{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.instance == nil {
		return "", checkpoint{}, status.Error(codes.FailedPrecondition, "VM not running")
	}
	cpID := NewID("cp")
	cp := checkpoint{Shape{e.vm.VcpuCount, e.vm.MemSizeMib, e.vm.ImageId}, id}
	if err := e.instance.Checkpoint(ctx, cpID); err != nil {
		_ = s.runtime.DeleteCheckpoint(cpID)
		return "", checkpoint{}, status.Errorf(codes.Internal, "checkpoint: %v", err)
	}
	return cpID, cp, nil
}
func (s *Server) CheckpointFirecrackerVM(ctx context.Context, r *api.CheckpointFirecrackerVMRequest) (*api.Checkpoint, error) {
	id, cp, err := s.capture(ctx, r.SailboxId)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.checkpoints[id] = cp
	s.mu.Unlock()
	return &api.Checkpoint{CheckpointId: id, SourceSailboxId: cp.source, VcpuCount: cp.shape.CPUs, MemSizeMib: cp.shape.Memory}, nil
}
func (s *Server) restore(ctx context.Context, id, cpID string, cp checkpoint) (*api.VM, error) {
	e, err := s.reserve(id, cp.shape)
	if err != nil {
		return nil, err
	}
	inst, err := s.runtime.Restore(ctx, e.vm.VmId, cpID, cp.shape)
	return s.activate(ctx, e, inst, err, true)
}
func (s *Server) RestoreFirecrackerVMFromCheckpoint(ctx context.Context, r *api.RestoreFirecrackerVMFromCheckpointRequest) (*api.VM, error) {
	s.mu.Lock()
	cp, ok := s.checkpoints[r.CheckpointId]
	s.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "checkpoint not found")
	}
	return s.restore(ctx, r.SailboxId, r.CheckpointId, cp)
}
func (s *Server) ForkFirecrackerVM(ctx context.Context, r *api.ForkFirecrackerVMRequest) (*api.VM, error) {
	// Reserve the child before pausing the parent. A capacity failure does not
	// interrupt an existing workload. Each lifecycle has its own lock.
	parent, err := s.lookup(r.SailboxId)
	if err != nil {
		return nil, err
	}
	shape := Shape{parent.vm.VcpuCount, parent.vm.MemSizeMib, parent.vm.ImageId}
	e, err := s.reserve(r.ChildSailboxId, shape)
	if err != nil {
		return nil, err
	}
	cpID, _, err := s.capture(ctx, r.SailboxId)
	if err != nil {
		e.mu.Unlock()
		s.release(e)
		return nil, err
	}
	defer s.runtime.DeleteCheckpoint(cpID)
	inst, err := s.runtime.Restore(ctx, e.vm.VmId, cpID, shape)
	return s.activate(ctx, e, inst, err, true)
}
func (s *Server) GetWorkerState(context.Context, *api.Empty) (*api.WorkerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &api.WorkerState{TotalVcpus: s.cpus, TotalMemoryMib: s.memory, ReservedVcpus: s.usedCPU, ReservedMemoryMib: s.usedMemory}
	for _, e := range s.vms {
		out.Vms = append(out.Vms, proto.Clone(e.vm).(*api.VM))
	}
	sort.Slice(out.Vms, func(i, j int) bool { return out.Vms[i].SailboxId < out.Vms[j].SailboxId })
	return out, nil
}
func (s *Server) guest(ctx context.Context, id string) (*grpc.ClientConn, error) {
	e, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.instance == nil {
		return nil, status.Error(codes.FailedPrecondition, "VM not ready")
	}
	return e.instance.Guest(ctx)
}
func (s *Server) StreamExec(r *api.StartExec, stream grpc.ServerStreamingServer[api.ExecServerFrame]) error {
	c, err := s.guest(stream.Context(), r.SailboxId)
	if err != nil {
		return err
	}
	defer c.Close()
	src, err := api.NewSailboxExecServiceClient(c).StreamExec(stream.Context(), r)
	if err != nil {
		return err
	}
	for {
		f, err := src.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err = stream.Send(f); err != nil {
			return err
		}
	}
}
func (s *Server) WriteExecStdin(ctx context.Context, r *api.WriteExecStdinRequest) (*api.Empty, error) {
	c, e := s.guest(ctx, r.SailboxId)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	return api.NewSailboxExecServiceClient(c).WriteExecStdin(ctx, r)
}
func (s *Server) ResizeExec(ctx context.Context, r *api.ResizeExecRequest) (*api.Empty, error) {
	c, e := s.guest(ctx, r.SailboxId)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	return api.NewSailboxExecServiceClient(c).ResizeExec(ctx, r)
}
func (s *Server) CancelExec(ctx context.Context, r *api.CancelExecRequest) (*api.Empty, error) {
	c, e := s.guest(ctx, r.SailboxId)
	if e != nil {
		return nil, e
	}
	defer c.Close()
	return api.NewSailboxExecServiceClient(c).CancelExec(ctx, r)
}
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	var ids []string
	for _, e := range s.vms {
		ids = append(ids, e.vm.VmId)
	}
	s.mu.Unlock()
	for _, id := range ids {
		if _, err := s.StopFirecrackerVM(ctx, &api.StopFirecrackerVMRequest{VmId: id}); err != nil {
			return err
		}
	}
	return nil
}
