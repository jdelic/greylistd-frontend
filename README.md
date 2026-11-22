# greylistd-frontend

A small, mobile-first web interface for
[greylistd](https://packages.debian.org/trixie/greylistd): list, filter and
sort grey-, white- and blacklisted entries, whitelist or blacklist them one by
one or in bulk, and add new domains or IPs — from your phone instead of an SSH
session.

- Talks to greylistd directly over its UNIX socket
- Login via OpenID Connect and/or a username and password
- Single static Go binary, runs in a `FROM scratch` container
- Configured via flags or `GREYLISTD_FRONTEND_*` environment variables
- JSON access log on stdout, JSON application log on stderr

```sh
docker run --user "$(id -u opensmtpd):$(id -g opensmtpd)" \
    -v /var/run/greylistd:/var/run/greylistd -p 8080:8080 \
    -e GREYLISTD_FRONTEND_AUTH_USERNAME=admin \
    -e GREYLISTD_FRONTEND_AUTH_PASSWORD=... \
    -e GREYLISTD_FRONTEND_SESSION_SECRET="$(openssl rand -hex 32)" \
    ghcr.io/jdelic/greylistd-frontend:latest
```

## Documentation

- [README.rst](README.rst) — configuration reference, login modes, building,
  CI/releases, security notes
- [deploy/systemd](deploy/systemd) — example systemd unit for the release
  binaries (static `linux/amd64` and `linux/arm64` builds on each GitHub
  release)
- [deploy/nomad](deploy/nomad/greylistd-frontend.nomad.hcl) — example Nomad job
- [.agent-docs/SPEC.rst](.agent-docs/SPEC.rst) — the original specification
- [.agent-docs/DESIGN.rst](.agent-docs/DESIGN.rst) — design document:
  implementation, assumptions and decisions
