# Running Pando on rootless Docker

On Linux, run Pando on rootless Docker (R-402). Pando's server holds the Docker socket so it can
start and stop apps, and whoever holds a socket controls that daemon. With rootful Docker, that
means a compromise of Pando is root on the host. With rootless Docker, it is the unprivileged user
the daemon runs as.

Docker Desktop, on macOS and Windows, already runs Docker inside a virtual machine, has no rootless
mode, and needs none of this.

## 1. Prepare the host

As root, once:

```bash
# Ubuntu 24.04 and later confine the unprivileged user namespaces rootless Docker needs.
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
echo 'kernel.apparmor_restrict_unprivileged_userns=0' | sudo tee /etc/sysctl.d/60-rootless-docker.conf

sudo apt-get install -y uidmap dbus-user-session docker-ce-rootless-extras

# Let the user's Docker apply CPU limits. systemd delegates only memory and
# pids to a user by default, and Docker drops a CPU limit it has no
# controller for.
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload

# Keep the user's services running when nobody is logged in.
sudo loginctl enable-linger pando
```

`pando` is the account Docker and Pando will run as: an ordinary user, not in the `docker` group.
If rootful Docker is installed, stop it with `sudo systemctl disable --now docker.service
docker.socket` so the two cannot be confused.

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
