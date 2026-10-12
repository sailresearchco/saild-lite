package host

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sailresearchco/saild-lite/api"
)

type blockedRuntime struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockedRuntime) Start(context.Context, string, Shape) (Instance, error) {
	r.started <- struct{}{}
	<-r.release
	return nil, errors.New("injected start failure")
}
func (r *blockedRuntime) Restore(context.Context, string, string, Shape) (Instance, error) {
	return nil, errors.New("unused")
}
func (r *blockedRuntime) DeleteCheckpoint(string) error { return nil }
func TestPendingStartsReserveAndFailuresRelease(t *testing.T) {
	r := &blockedRuntime{make(chan struct{}, 2), make(chan struct{})}
	s := New(r, 2, 1024)
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.StartFirecrackerVM(context.Background(), &api.StartFirecrackerVMRequest{SailboxId: id, VcpuCount: 1, MemSizeMib: 512, ImageId: "default"})
			if e == nil {
				t.Error("expected injected error")
			}
		}()
	}
	<-r.started
	<-r.started
	if _, e := s.StartFirecrackerVM(context.Background(), &api.StartFirecrackerVMRequest{SailboxId: "three", VcpuCount: 1, MemSizeMib: 512, ImageId: "default"}); e == nil {
		t.Fatal("over-admitted")
	}
	state, _ := s.GetWorkerState(context.Background(), &api.Empty{})
	if state.ReservedVcpus != 2 || state.ReservedMemoryMib != 1024 {
		t.Fatalf("pending reservations missing: %v", state)
	}
	close(r.release)
	wg.Wait()
	state, _ = s.GetWorkerState(context.Background(), &api.Empty{})
	if len(state.Vms) != 0 || state.ReservedMemoryMib != 0 || state.ReservedVcpus != 0 {
		t.Fatalf("leaked failed admission: %v", state)
	}
}
