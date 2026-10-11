#!/bin/sh
# Turns a GitHub-hosted ubuntu-24.04 runner into a rootless Podman host, the
# way docs/rootless.md sets one up, and points the job's DOCKER_HOST at
# Podman's Docker-compatible socket (R-402, issue #179). The runner's Docker
# daemon is stopped; its CLI and Compose stay and drive Podman.
#
# usage: scripts/ci-rootless-podman.sh   (in a GitHub Actions step)
set -eux

# Pando's build container runs rootless BuildKit, which Ubuntu's AppArmor
# confinement of unprivileged user namespaces blocks (docs/rootless.md).
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
sudo apt-get update -q
sudo apt-get install -y -q podman uidmap dbus-user-session

# systemd delegates only memory and pids to a user by default.
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload
sudo loginctl enable-linger "$USER"
sudo systemctl restart "user@$(id -u).service"

# Docker treats a registry on 127.0.0.0/8 as plain HTTP; Podman does not, and
# the adapter's private-image test serves one there.
printf '[[registry]]\nlocation = "127.0.0.1"\ninsecure = true\n' | sudo tee /etc/containers/registries.conf.d/pando-test.conf

sudo systemctl disable --now docker.service docker.socket

export XDG_RUNTIME_DIR="/run/user/$(id -u)"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
systemctl --user enable --now podman.socket
podman --version
podman info --format '{{.Host.Security.Rootless}} cgroup={{.Host.CgroupsVersion}} controllers={{.Host.CgroupControllers}}'
podman info --format '{{.Host.Security.Rootless}}' | grep -qx true

echo "XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR" >> "$GITHUB_ENV"
echo "DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS" >> "$GITHUB_ENV"
echo "DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock" >> "$GITHUB_ENV"
echo "PANDO_DOCKER_SOCKET=$XDG_RUNTIME_DIR/podman/podman.sock" >> "$GITHUB_ENV"
