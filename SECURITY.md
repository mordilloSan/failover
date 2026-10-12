# Security

## Reporting a vulnerability

Please do not open a public issue. Report it privately through
**Security → Report a vulnerability** on this repository.

## Scope

This repository covers the failover agent: the Go code, its web UI and login,
the Dockerfile and image, the compose file, the end-to-end scripts and the
workflows.

Vulnerabilities in the software it drives belong to their own projects:
Docker and Compose, Btrfs, Technitium DNS, Nginx Proxy Manager, the mail
server, and the services it moves between the server and the TNAS.

## By design

The agent runs privileged, with the Docker socket and `/Volume1` mounted: it
needs them to take Btrfs snapshots and start containers. Its UI is HTTPS with a
self-signed certificate the agent renews, meant for the LAN or WireGuard only.
Neither is a vulnerability on its own. A way to reach them from outside that
setup is.

## Supported versions

Only the latest release, the `latest` tag, receives fixes.
