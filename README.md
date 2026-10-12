# saild-lite

saild-lite runs Linux Firecracker VMs on one host. It supports command execution, shells, and VM copies with RAM.
Use this system with trusted operators. It does not support Windows or production workloads.

## Start

In the repository directory, install the tools and CLI:

```sh
just setup
```

If `just` is absent, run `bash scripts/setup.sh` instead. Use macOS or Linux. On Windows, use WSL.
Open a new terminal to use the updated PATH. Return to the repository directory.
Configure an AWS profile. Then create the host:

```sh
pulumi login
AWS_PROFILE=your-profile just provision
just tunnel
```

Keep the tunnel open. In another terminal, run `sail-lite tui` to create and control VMs.

## Deploy and test

**A deployment or service restart deletes all VMs and checkpoints.**

```sh
just redeploy                        # Build, replace binaries, and reset VM state.
just test                            # Run local tests.
```

Redeploy updates the guest agent and init script. To apply Dockerfile changes, stop the service.
Remove `/opt/saild-lite/assets/default.ext4`. Then run `just bootstrap`.

To read service logs, run `just ssh`. Then run `sudo journalctl -u saild-lite -n 100`.

## Use another host

Use Ubuntu 24.04 on x86-64 with systemd, `/dev/kvm`, internet access, and passwordless sudo.
If the host is a VM, enable nested virtualization.

```sh
export SAILD_HOST=your-host
export SAILD_USER=ubuntu
export SAILD_SSH_KEY="$HOME/.ssh/your-key"
just bootstrap
just tunnel
```

These settings replace the values in `artifacts/host.json`. Deployment uses SSH. It does not require AWS.
The [Pulumi example](infra/Pulumi.yaml) creates a `c8i.xlarge` host in `us-west-2`. AWS charges apply.
By default, a timer terminates that host after 24 hours. The default timer does not survive a reboot.
Use `AWS_PROFILE=your-profile just destroy` to remove all cloud resources for the stack.

## Architecture

`sail-lite` → gRPC through SSH → `saild` → gRPC through vsock → `guest-agent` → command process.

| Code | Function |
| --- | --- |
| [host](internal/host/server.go) | Control VM state and CPU/RAM reservations. Forward commands to the guest. |
| [firecracker](internal/firecracker/runtime_linux.go) | Control Firecracker through Unix sockets. Manage disks, networks, and snapshots. |
| [guest](internal/guest/server.go) | Run commands. Control input, output, terminals, and guest identity. |

The host reserves CPU and RAM before VM creation. Each VM has a separate disk and network namespace.
Fork pauses the parent, copies its RAM and disk, then resumes it. The child starts from the copy with a new ID.
Existing processes keep their environment. New commands use the new ID. `/run/sailbox-id` contains the current ID.
External connections can break after a fork. Connect to the child again.
State stays in memory and on local disks. There is no database or shared storage. Checkpoints stay until reset.

## API

The host exposes gRPC at `127.0.0.1:50051`. Use the SSH tunnel for access.
Set `SAILD_ADDR` to select another address. There is no REST API or gRPC authentication.
The [protobuf file](proto/saild.proto) defines all fields. This reduced API is not compatible with the full production API.

| Service | RPC | Function |
| --- | --- | --- |
| `FirecrackerControlService` | `StartFirecrackerVM` | Create a VM. Return its Sailbox ID and VM ID. |
| | `StopFirecrackerVM` | Stop a VM by VM ID. |
| | `ForkFirecrackerVM` | Copy a running Sailbox into a new Sailbox ID. |
| | `CheckpointFirecrackerVM` | Save RAM, disk, and machine state. Return a checkpoint ID. |
| | `RestoreFirecrackerVMFromCheckpoint` | Create a Sailbox from a checkpoint ID. |
| | `GetWorkerState` | Return VMs and total/reserved CPU and RAM. |
| `SailboxExecService` | `StreamExec` | Start a command or connect to an existing command. Return start, output, and exit frames. |
| | `WriteExecStdin` | Send input or EOF. |
| | `ResizeExec` | Change terminal rows and columns. |
| | `CancelExec` | Kill the command process group. |

Exec requests use a Sailbox ID and an exec request ID. Commands continue after a client disconnects.
Clients can request stored output with sequence numbers. Each session stores up to 1 MiB. Each guest holds up to 64 sessions.
The guest also exposes `Rebind`, `Sync`, and `Ping` to the host through vsock. Clients do not call these methods through saild.
