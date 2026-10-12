//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"github.com/sailresearchco/saild-lite/internal/firecracker"
	"github.com/sailresearchco/saild-lite/internal/host"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	mode := "serve"
	if len(os.Args) > 1 && os.Args[1] == "reset" {
		mode = "reset"
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	state := flag.String("state", "/var/lib/saild-lite", "mutable state directory")
	assets := flag.String("assets", "/opt/saild-lite/assets", "immutable image directory")
	fc := flag.String("firecracker", "/opt/saild-lite/firecracker", "Firecracker binary")
	addr := flag.String("listen", "127.0.0.1:50051", "local gRPC address")
	cpus := flag.Uint("cpus", 8, "host vCPU admission budget")
	memory := flag.Uint("memory", 4096, "host RAM admission budget in MiB")
	flag.Parse()
	if os.Geteuid() != 0 {
		return fmt.Errorf("saild requires root for KVM, namespaces and chroot")
	}
	if err := os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(*state, "host.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("host already active; stop saild-lite.service before reset: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if mode == "reset" {
		return firecracker.Reset(ctx, *state)
	}
	ip, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || !parsed.IsLoopback() {
		return fmt.Errorf("listen must be loopback; use SSH forwarding for remote clients")
	}
	r := &firecracker.Runtime{State: *state, Assets: *assets, Binary: *fc}
	if err = r.Init(ctx); err != nil {
		return err
	}
	s := host.New(r, uint32(*cpus), uint32(*memory))
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer lis.Close()
	g := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		// Bound lifecycle calls even when a client omits a deadline.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return next(ctx, req)
	}))
	api.RegisterFirecrackerControlServiceServer(g, s)
	api.RegisterSailboxExecServiceServer(g, s)
	h := health.NewServer()
	healthpb.RegisterHealthServer(g, h)
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	reflection.Register(g)
	stopCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() { <-stopCtx.Done(); h.Shutdown(); g.Stop() }()
	log.Printf("saild-lite serving %s", *addr)
	err = g.Serve(lis)
	shutdown, finish := context.WithTimeout(context.Background(), 15*time.Second)
	defer finish()
	if closeErr := s.Close(shutdown); closeErr != nil {
		return closeErr
	}
	return err
}
