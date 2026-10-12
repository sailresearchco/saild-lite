FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates curl iproute2 procps python3 tini && rm -rf /var/lib/apt/lists/*
COPY guest-agent /usr/local/bin/guest-agent
COPY init /init
RUN chmod 0755 /init /usr/local/bin/guest-agent && mkdir -p /dev/pts /dev/shm /proc /sys /run /tmp && chmod 1777 /tmp
