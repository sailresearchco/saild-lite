#!/usr/bin/env bash
set -euo pipefail
mode="$1"
stage="$2"
[[ "$stage" =~ ^/tmp/saild-lite-upload-[a-f0-9]+$ ]]
[[ "$mode" == bootstrap || "$mode" == redeploy ]]
exec 9>/var/lock/saild-lite-deploy.lock
flock -n 9 || { echo 'another deployment is running' >&2; exit 1; }
test -c /dev/kvm
source "$stage/artifacts.env"
prefix=/opt/saild-lite
mkdir -p "$prefix/assets" /var/lib/saild-lite

if [[ "$mode" == bootstrap ]]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y docker.io iproute2 iptables e2fsprogs curl ca-certificates
  systemctl start docker
  curl -fLsS "$FIRECRACKER_URL" -o "$stage/firecracker.tgz"
  echo "$FIRECRACKER_SHA256  $stage/firecracker.tgz" | sha256sum -c -
  tar -xzf "$stage/firecracker.tgz" -C "$stage"
  install -m 0755 "$stage/release-v1.17.0-x86_64/firecracker-v1.17.0-x86_64" "$prefix/firecracker.next"
  curl -fLsS "$KERNEL_URL" -o "$stage/vmlinux"
  echo "$KERNEL_SHA256  $stage/vmlinux" | sha256sum -c -
  install -m 0644 "$stage/vmlinux" "$prefix/assets/vmlinux.next"
fi

# Prepare a replacement image before stopping the old daemon. Every VM owns a
# separate writable copy, so preparing this base cannot change a live disk.
candidate="$prefix/assets/default.ext4.next"
if [[ ! -f "$prefix/assets/default.ext4" ]]; then
  cp "$stage/guest-agent-linux" "$stage/guest-agent"
  docker build -t saild-lite-rootfs "$stage"
  container=$(docker create saild-lite-rootfs)
  trap 'docker rm -f "$container" >/dev/null 2>&1 || true' EXIT
  mkdir "$stage/rootfs"
  docker export "$container" | tar -xf - -C "$stage/rootfs"
  truncate -s 2G "$candidate"
  mkfs.ext4 -q -F -d "$stage/rootfs" "$candidate"
  docker rm "$container" >/dev/null
  trap - EXIT
else
  cp --reflink=auto --sparse=always "$prefix/assets/default.ext4" "$candidate"
  mkdir "$stage/mount"
  mount -o loop "$candidate" "$stage/mount"
  trap 'umount "$stage/mount"' EXIT
  install -m 0755 "$stage/guest-agent-linux" "$stage/mount/usr/local/bin/guest-agent"
  install -m 0755 "$stage/init" "$stage/mount/init"
  umount "$stage/mount"
  trap - EXIT
fi

install -m 0755 "$stage/saild-linux" "$prefix/saild.next"
install -m 0755 "$stage/sail-linux" "$prefix/sail.next"
if systemctl cat saild-lite.service >/dev/null 2>&1; then
  systemctl stop saild-lite.service
fi
# systemd's KillMode=control-group ends every VMM before accounting is cleared.
"$prefix/saild.next" reset
if [[ -f "$prefix/saild" ]]; then cp "$prefix/saild" "$prefix/saild.previous"; fi
mv "$prefix/saild.next" "$prefix/saild"
mv "$prefix/sail.next" "$prefix/sail"
mv "$candidate" "$prefix/assets/default.ext4"
if [[ -f "$prefix/firecracker.next" ]]; then mv "$prefix/firecracker.next" "$prefix/firecracker"; fi
if [[ -f "$prefix/assets/vmlinux.next" ]]; then mv "$prefix/assets/vmlinux.next" "$prefix/assets/vmlinux"; fi
if [[ ! -f /etc/default/saild-lite ]]; then
  printf 'SAILD_CPUS=%s\nSAILD_MEMORY_MIB=4096\n' "$(nproc)" > /etc/default/saild-lite
fi
install -m 0644 "$stage/saild-lite.service" /etc/systemd/system/saild-lite.service
systemctl daemon-reload
systemctl enable --now saild-lite.service
for attempt in $(seq 1 30); do
  if "$prefix/sail" list --json; then
    echo 'saild-lite deployed; VM accounting is empty'
    exit 0
  fi
  sleep 1
done
journalctl -u saild-lite.service -n 80 --no-pager
exit 1
