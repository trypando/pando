# Running Pando on rootless Docker or Podman

On Linux, run Pando on rootless Docker (R-402), or on rootless [Podman](#podman). Pando's server holds the Docker socket so it can
start and stop apps, and whoever holds a socket controls that daemon. With rootful Docker, that
means a compromise of Pando is root on the host. With rootless Docker, it is the unprivileged user
the daemon runs as.

Docker Desktop, on macOS and Windows, already runs Docker inside a virtual machine, has no rootless
mode, and needs none of this.

## 1. Prepare the host

As root, once. The commands are for Ubuntu; on another distribution, follow Docker's install guide
for it, then continue from the `Delegate` step.

```bash
# The account Docker and Pando will run as: an ordinary user, not in the docker group.
sudo useradd --create-home --shell /bin/bash pando

# Ubuntu 24.04 and later confine unprivileged user namespaces. Rootless Docker does not need this
# line: Ubuntu's apparmor package allows them for /usr/bin/rootlesskit. Pando's build container
# does: it runs rootless BuildKit, whose rootlesskit that profile does not cover.
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-rootless-docker.conf

# Docker's own apt repository: Ubuntu's docker.io package has no rootless extras.
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update
sudo apt-get install -y uidmap dbus-user-session docker-ce docker-ce-cli containerd.io docker-compose-plugin docker-ce-rootless-extras

# Let the user's Docker apply CPU limits. systemd delegates only memory and
# pids to a user by default, and Docker drops a CPU limit it has no
# controller for.
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload

# Keep the user's services running when nobody is logged in.
sudo loginctl enable-linger pando
```

To use an existing account instead of `pando`, skip `useradd` and put its name wherever `pando`
appears in this guide. The packages above install rootful Docker too; stop it with
`sudo systemctl disable --now docker.service docker.socket` so the two cannot be confused.

## 2. Install rootless Docker

As that user, in a login session (`sudo machinectl shell pando@` works where `su` does not):

```bash
dockerd-rootless-setuptool.sh install
systemctl --user enable docker
docker info --format '{{json .SecurityOptions}} cpu={{.CPUCfsQuota}} memory={{.MemoryLimit}}'
```

The last line should include `name=rootless`, `cpu=true` and `memory=true`. If `cpu` is false, the
delegation in step 1 has not reached the user's session: run `sudo systemctl restart user@$(id -u
pando).service`, then `systemctl --user restart docker`.

## 3. Install Pando

As the same user, follow the README's install, adding one line to `.env` beside the compose file
before the first start:

```bash
echo "PANDO_DOCKER_SOCKET=$XDG_RUNTIME_DIR/docker.sock" >> .env   # usually /run/user/1000/docker.sock
docker compose up -d
```

`PANDO_DOCKER_SOCKET` is the socket Pando's container mounts. Without it, Compose mounts
`/var/run/docker.sock`, which is rootful Docker's.

## 4. Check

In the console, open **Installation**, then **Capacity**, then **Show details** for the Docker
runtime. `rootless` should be `true`, and `cpu_limits` and `memory_limits` should be `true` too.

Pando refuses to deploy to a Docker that would drop the CPU or memory limits every app has (R-240).
The refusal says which limit and how to delegate it, so a host where step 1 was missed fails at the
first deploy, not silently.

## Podman

Pando also runs on rootless Podman, through Podman's Docker-compatible socket. These steps are
tested with Podman 4.9 on Ubuntu 24.04, the same way as the Docker steps above.

### 1. Prepare the host

As root, once:

```bash
# The account Podman and Pando will run as.
sudo useradd --create-home --shell /bin/bash pando

# Pando's build container runs rootless BuildKit, which Ubuntu 24.04's confinement of unprivileged
# user namespaces blocks.
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-rootless-podman.conf

sudo apt-get update
sudo apt-get install -y podman uidmap dbus-user-session

# Docker's CLI and its Compose plugin, from Docker's apt repository, to run Pando's compose file
# against Podman. Not Docker's daemon.
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list
sudo apt-get update
sudo apt-get install -y docker-ce-cli docker-compose-plugin

# Let the user's Podman apply CPU limits; systemd delegates only memory and pids by default.
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload

sudo loginctl enable-linger pando
```

### 2. Start Podman's socket

As that user, in a login session (`sudo machinectl shell pando@`):

```bash
systemctl --user enable --now podman.socket
echo 'export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock' >> ~/.bashrc
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock
podman info --format '{{.Host.Security.Rootless}} {{.Host.CgroupControllers}}'
```

The last line should print `true` and a list that includes `cpu` and `memory`. If `cpu` is missing,
run `sudo systemctl restart user@$(id -u pando).service` and log in again.

### 3. Install Pando

As the same user. Use the release's compose file, which names the published image: Compose cannot
build Pando's image through Podman.

```bash
mkdir ~/pando && cd ~/pando
curl -fsSLO https://github.com/trypando/pando/releases/latest/download/docker-compose.yml
echo "PANDO_DOCKER_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock" >> .env   # usually /run/user/1000/podman/podman.sock
docker compose up -d
```

### 4. Start Pando at boot

Podman has no daemon to start containers again after a reboot, so a user service runs Compose:

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/pando.service <<'EOF'
[Unit]
Description=Pando
Requires=podman.socket
After=podman.socket

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=%h/pando
Environment=DOCKER_HOST=unix://%t/podman/podman.sock
ExecStart=/usr/bin/docker compose up -d

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable pando.service
```

Pando starts the apps it runs once it is up.

### 5. Check

As step 4 of the Docker steps: under **Installation**, **Capacity**, the Docker runtime's details
show `rootless`, `cpu_limits` and `memory_limits` as `true`. Pando reads them from Podman's own
`podman info`, and refuses to deploy while either limit would be dropped, saying how to delegate it.

### What changes under Podman

- **Ports 80 and 443** and **disk** are as under rootless Docker, below; Podman keeps images,
  containers and volumes under `~/.local/share/containers`. Check one sign-in's address in the audit
  log after installing, as there.
- **Pando's own outbound connections.** Podman gives every network a container joins an equal
  default route, so Pando's container sends its own traffic out (cloning repositories, for example)
  through the most recently joined app's network. It still leaves through Podman's NAT, as Docker's
  does; only the network differs.
- **A registry without TLS.** Docker reads a registry on `127.0.0.1` as plain HTTP. Podman does not;
  list it in `/etc/containers/registries.conf.d/` with `insecure = true`.
- **`oci_runtime` is refused.** Podman's Docker-compatible API accepts a runtime such as gVisor and
  does not apply it, so the sandboxed isolation class is Docker only.

## What changes under rootless Docker

- **Ports 80 and 443.** An unprivileged daemon cannot bind them, so the Traefik edge fails to start
  on them. Either allow it, with `sudo sysctl -w net.ipv4.ip_unprivileged_port_start=80` (and the
  same line in `/etc/sysctl.d/`), or give the edge 8080 and 8443 and forward to them.
- **Client addresses.** Docker's default rootless port forwarding hides the client's address, so the
  audit log would record the forwarder's. Docker's rootless documentation, under "Networking", gives
  the setting that keeps it. Check one sign-in's address in the audit log after installing.
- **A model on the host.** `host.docker.internal` reaches the rootless daemon's own network
  namespace, not the host. A model served on the host, for the local AI adapter, is only reachable
  if Docker's rootless setup is told to allow host loopback.
- **Disk.** Images, containers and volumes live in the user's home directory, under
  `~/.local/share/docker`. Give that its own filesystem if you want apps unable to fill the host's.

Docker's own guide, which these steps follow, is at
<https://docs.docker.com/engine/security/rootless/>.
