//go:build linux

// Package firecracker implements local full snapshots. Every VMM has its own
// chroot and network namespace, so snapshot-embedded paths, IPs and MACs are
// stable while their host resources and writable disks remain independent.
package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sailresearchco/saild-lite/api"
	"github.com/sailresearchco/saild-lite/internal/host"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Runtime struct {
	State, Assets, Binary string
	mu                    sync.Mutex
	slots                 [255]bool
}
type instance struct {
	r       *Runtime
	dir, ns string
	slot    int
	cmd     *exec.Cmd
	done    chan struct{}
	client  *http.Client
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
func copyFile(ctx context.Context, src, dst string) error {
	return run(ctx, "cp", "--reflink=auto", "--sparse=always", "--", src, dst)
}
func (r *Runtime) allocate() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 1; i < len(r.slots); i++ {
		if !r.slots[i] {
			r.slots[i] = true
			return i, nil
		}
	}
	return 0, errors.New("network slots exhausted")
}
func (r *Runtime) free(slot int) { r.mu.Lock(); r.slots[slot] = false; r.mu.Unlock() }

// Init requires an otherwise empty owned state directory. Service startup and
// redeploy run Reset after the old service cgroup has been killed.
func (r *Runtime) Init(ctx context.Context) error {
	for _, p := range []string{r.State, r.Assets, r.Binary} {
		if !filepath.IsAbs(p) {
			return errors.New("state, assets and binary paths must be absolute")
		}
	}
	for _, dir := range []string{"vms", "checkpoints"} {
		if err := os.MkdirAll(filepath.Join(r.State, dir), 0700); err != nil {
			return err
		}
	}
	if err := run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}
	// Dedicated chains are never flushed while VMs exist. Only our hook/rules
	// are removed by Reset; other host workloads' rules are untouched.
	for _, c := range [][]string{
		{"-t", "nat", "-N", "SDLITE_NAT"}, {"-N", "SDLITE_FWD"}, {"-N", "SDLITE_INPUT"},
	} {
		if err := run(ctx, "iptables", c...); err != nil {
			return err
		}
	}
	for _, c := range [][]string{
		{"-t", "nat", "-A", "SDLITE_NAT", "-s", "10.231.0.0/16", "-j", "MASQUERADE"},
		{"-t", "nat", "-A", "POSTROUTING", "-j", "SDLITE_NAT"},
		{"-A", "SDLITE_FWD", "-i", "sdlh+", "-o", "sdlh+", "-j", "DROP"},
		{"-A", "SDLITE_FWD", "-i", "sdlh+", "-d", "169.254.169.254/32", "-j", "DROP"},
		{"-A", "SDLITE_FWD", "-i", "sdlh+", "-j", "ACCEPT"},
		{"-A", "SDLITE_FWD", "-o", "sdlh+", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-I", "FORWARD", "1", "-j", "SDLITE_FWD"},
		{"-A", "SDLITE_INPUT", "-i", "sdlh+", "-j", "DROP"},
		{"-I", "INPUT", "1", "-j", "SDLITE_INPUT"},
	} {
		if err := run(ctx, "iptables", c...); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runtime) prepare(ctx context.Context, id string) (*instance, error) {
	slot, err := r.allocate()
	if err != nil {
		return nil, err
	}
	i := &instance{r: r, dir: filepath.Join(r.State, "vms", id), ns: fmt.Sprintf("sdl-%d", slot), slot: slot, done: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			i.cleanup()
			r.free(slot)
		}
	}()
	if err := os.Mkdir(i.dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(i.dir, "dev"), 0755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(i.dir, "dev/net"), 0755); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		name         string
		major, minor uint32
	}{{"kvm", 10, 232}, {"net/tun", 10, 200}, {"null", 1, 3}, {"urandom", 1, 9}} {
		if err := unix.Mknod(filepath.Join(i.dir, "dev", d.name), unix.S_IFCHR|0600, int(unix.Mkdev(d.major, d.minor))); err != nil {
			return nil, err
		}
	}
	if err := copyFile(ctx, r.Binary, filepath.Join(i.dir, "firecracker")); err != nil {
		return nil, err
	}
	if err := i.network(ctx); err != nil {
		return nil, err
	}
	i.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(i.dir, "api.sock"))
	}}}
	success = true
	return i, nil
}
func (i *instance) network(ctx context.Context) error {
	hostLink := fmt.Sprintf("sdlh%d", i.slot)
	peer := fmt.Sprintf("sdlp%d", i.slot)
	hostIP := fmt.Sprintf("10.231.%d.1", i.slot)
	nsIP := fmt.Sprintf("10.231.%d.2", i.slot)
	commands := [][]string{
		{"netns", "add", i.ns}, {"link", "add", hostLink, "type", "veth", "peer", "name", peer},
		{"link", "set", peer, "netns", i.ns}, {"addr", "add", hostIP + "/30", "dev", hostLink}, {"link", "set", hostLink, "up"},
		{"-n", i.ns, "link", "set", peer, "name", "veth0"}, {"-n", i.ns, "addr", "add", nsIP + "/30", "dev", "veth0"},
		{"-n", i.ns, "link", "set", "veth0", "up"}, {"-n", i.ns, "link", "set", "lo", "up"},
		{"-n", i.ns, "route", "add", "default", "via", hostIP},
		{"netns", "exec", i.ns, "ip", "tuntap", "add", "dev", "tap0", "mode", "tap"},
		{"-n", i.ns, "addr", "add", "172.30.0.1/24", "dev", "tap0"}, {"-n", i.ns, "link", "set", "tap0", "up"},
		{"netns", "exec", i.ns, "sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"netns", "exec", i.ns, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", "172.30.0.0/24", "-o", "veth0", "-j", "MASQUERADE"},
	}
	for _, cmd := range commands {
		if err := run(ctx, "ip", cmd...); err != nil {
			return err
		}
	}
	return nil
}
func (i *instance) api(ctx context.Context, method, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return fmt.Errorf("firecracker %s %s: %d %s", method, path, res.StatusCode, b)
	}
	return nil
}
func (i *instance) launch(ctx context.Context) error {
	log, err := os.OpenFile(filepath.Join(i.dir, "console.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	i.cmd = exec.Command("ip", "netns", "exec", i.ns, "chroot", i.dir, "/firecracker", "--api-sock", "/api.sock")
	i.cmd.Stdout = log
	i.cmd.Stderr = log
	i.cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := i.cmd.Start(); err != nil {
		return err
	}
	go func() {
		_ = i.cmd.Wait()
		i.client.CloseIdleConnections()
		i.cleanup()
		i.r.free(i.slot)
		close(i.done)
	}()
	return retry(ctx, i.done, func() error {
		c, e := net.DialTimeout("unix", filepath.Join(i.dir, "api.sock"), time.Second)
		if e == nil {
			c.Close()
		}
		return e
	})
}
func retry(ctx context.Context, done <-chan struct{}, fn func() error) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	var last error
	for {
		if err := fn(); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ctx.Err(), last)
		case <-done:
			return fmt.Errorf("VMM exited: %v", last)
		case <-t.C:
		}
	}
}
func (r *Runtime) Start(ctx context.Context, id string, shape host.Shape) (host.Instance, error) {
	i, err := r.prepare(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = copyFile(ctx, filepath.Join(r.Assets, shape.Image+".ext4"), filepath.Join(i.dir, "rootfs.ext4")); err != nil {
		i.cleanup()
		r.free(i.slot)
		return nil, err
	}
	if err = copyFile(ctx, filepath.Join(r.Assets, "vmlinux"), filepath.Join(i.dir, "vmlinux")); err != nil {
		i.cleanup()
		r.free(i.slot)
		return nil, err
	}
	if err = i.launch(ctx); err != nil {
		return r.failed(i, err)
	}
	for _, a := range []struct {
		path string
		body any
	}{
		{"/machine-config", map[string]any{"vcpu_count": shape.CPUs, "mem_size_mib": shape.Memory, "track_dirty_pages": false}},
		{"/boot-source", map[string]any{"kernel_image_path": "/vmlinux", "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": false, "io_engine": "Sync"}},
		{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02"}},
		{"/vsock", map[string]any{"guest_cid": 3, "uds_path": "/vsock.sock"}},
		{"/actions", map[string]any{"action_type": "InstanceStart"}},
	} {
		if err = i.api(ctx, "PUT", a.path, a.body); err != nil {
			return r.failed(i, err)
		}
	}
	return i, nil
}
func (r *Runtime) failed(i *instance, err error) (host.Instance, error) {
	if i.cmd == nil || i.cmd.Process == nil {
		i.cleanup()
		r.free(i.slot)
		return nil, err
	}
	// Keep failed-but-live instances visible to the host until Stop succeeds.
	return i, err
}
func (r *Runtime) Restore(ctx context.Context, id, cpID string, shape host.Shape) (host.Instance, error) {
	i, err := r.prepare(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"rootfs.ext4", "snapshot.mem", "snapshot.state"} {
		if err = copyFile(ctx, filepath.Join(r.State, "checkpoints", cpID, name), filepath.Join(i.dir, name)); err != nil {
			i.cleanup()
			r.free(i.slot)
			return nil, err
		}
	}
	if err = i.launch(ctx); err != nil {
		return r.failed(i, err)
	}
	err = i.api(ctx, "PUT", "/snapshot/load", map[string]any{"snapshot_path": "/snapshot.state", "mem_backend": map[string]any{"backend_path": "/snapshot.mem", "backend_type": "File"}, "enable_diff_snapshots": false, "resume_vm": true})
	if err != nil {
		return r.failed(i, err)
	}
	return i, nil
}
func (i *instance) Checkpoint(ctx context.Context, id string) (result error) {
	conn, err := i.Guest(ctx)
	if err != nil {
		return err
	}
	_, err = api.NewGuestAgentServiceClient(conn).Sync(ctx, &api.Empty{})
	conn.Close()
	if err != nil {
		return err
	}
	if err = i.api(ctx, "PATCH", "/vm", map[string]string{"state": "Paused"}); err != nil {
		// A lost reply can mean the pause took effect. Do not leave an
		// unaccounted paused VM behind after an ambiguous lifecycle failure.
		_ = i.cmd.Process.Kill()
		return err
	}
	// Snapshot I/O cannot be cancelled halfway through and then safely resume.
	// Use a bounded independent context; a timeout kills the VMM instead of
	// freeing admission while a hypervisor may still be writing or running.
	defer func() {
		resumeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := i.api(resumeCtx, "PATCH", "/vm", map[string]string{"state": "Resumed"}); err != nil {
			_ = i.cmd.Process.Kill()
			result = errors.Join(result, fmt.Errorf("resume source: %w", err))
		}
	}()
	captureCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err = i.api(captureCtx, "PUT", "/snapshot/create", map[string]string{"snapshot_type": "Full", "snapshot_path": "/capture.state", "mem_file_path": "/capture.mem"}); err != nil {
		if captureCtx.Err() != nil {
			_ = i.cmd.Process.Kill()
		}
		return err
	}
	dir := filepath.Join(i.r.State, "checkpoints", id)
	if err = os.Mkdir(dir, 0700); err != nil {
		return err
	}
	if err = copyFile(captureCtx, filepath.Join(i.dir, "rootfs.ext4"), filepath.Join(dir, "rootfs.ext4")); err != nil {
		return err
	}
	for _, suffix := range []string{"state", "mem"} {
		if err = os.Rename(filepath.Join(i.dir, "capture."+suffix), filepath.Join(dir, "snapshot."+suffix)); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (i *instance) Guest(ctx context.Context) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(i.dir, "vsock.sock"))
		if err != nil {
			return nil, err
		}
		deadline := time.Now().Add(3 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = c.SetDeadline(deadline)
		if _, err = io.WriteString(c, "CONNECT 5000\n"); err != nil {
			c.Close()
			return nil, err
		}
		// Read one byte at a time: do not consume any HTTP/2 bytes past the
		// handshake. Firecracker's response is an ASCII OK plus the host port.
		var b strings.Builder
		one := make([]byte, 1)
		for b.Len() < 128 {
			if _, err = io.ReadFull(c, one); err != nil {
				c.Close()
				return nil, err
			}
			b.WriteByte(one[0])
			if one[0] == '\n' {
				break
			}
		}
		if !strings.HasPrefix(b.String(), "OK ") {
			c.Close()
			return nil, fmt.Errorf("vsock handshake: %q", b.String())
		}
		_ = c.SetDeadline(time.Time{})
		return c, nil
	}))
	if err != nil {
		return nil, err
	}
	err = retry(ctx, i.done, func() error {
		call, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, e := api.NewGuestAgentServiceClient(conn).Ping(call, &api.Empty{})
		return e
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func (i *instance) Done() <-chan struct{} { return i.done }
func (i *instance) Stop(ctx context.Context) error {
	select {
	case <-i.done:
		return nil
	default:
	}
	if err := i.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (i *instance) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = run(ctx, "ip", "netns", "del", i.ns)
	_ = run(ctx, "ip", "link", "del", fmt.Sprintf("sdlh%d", i.slot))
	// Keep console output outside the VM directory for post-mortem inspection.
	_ = os.MkdirAll(filepath.Join(i.r.State, "logs"), 0700)
	_ = os.Rename(filepath.Join(i.dir, "console.log"), filepath.Join(i.r.State, "logs", filepath.Base(i.dir)+".log"))
	_ = os.RemoveAll(i.dir)
}
func (r *Runtime) DeleteCheckpoint(id string) error {
	return os.RemoveAll(filepath.Join(r.State, "checkpoints", id))
}

// Reset must run after systemd has stopped the entire service cgroup. It never
// kills arbitrary PIDs, flushes global firewall chains or removes base images.
func Reset(ctx context.Context, state string) error {
	if !filepath.IsAbs(state) || state == "/" || len(filepath.Clean(state)) < 8 {
		return errors.New("unsafe state directory")
	}
	out, err := exec.CommandContext(ctx, "ip", "netns", "list").Output()
	if err != nil {
		return err
	}
	scan := bufio.NewScanner(bytes.NewReader(out))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) > 0 && strings.HasPrefix(fields[0], "sdl-") {
			if err = run(ctx, "ip", "netns", "del", fields[0]); err != nil {
				return err
			}
		}
	}
	for slot := 1; slot < 255; slot++ {
		_ = run(ctx, "ip", "link", "del", fmt.Sprintf("sdlh%d", slot))
	}
	for _, c := range []struct{ table, hook, chain string }{{"nat", "POSTROUTING", "SDLITE_NAT"}, {"filter", "FORWARD", "SDLITE_FWD"}, {"filter", "INPUT", "SDLITE_INPUT"}} {
		_ = run(ctx, "iptables", "-t", c.table, "-D", c.hook, "-j", c.chain)
		_ = run(ctx, "iptables", "-t", c.table, "-F", c.chain)
		_ = run(ctx, "iptables", "-t", c.table, "-X", c.chain)
	}
	for _, name := range []string{"vms", "checkpoints"} {
		if err = os.RemoveAll(filepath.Join(state, name)); err != nil {
			return err
		}
	}
	return nil
}
