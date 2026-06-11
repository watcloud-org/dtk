# Security Policy

## Docker socket access

dtk requires access to the Docker socket (`/var/run/docker.sock`), which grants full control over the Docker daemon. To reduce the attack surface:

- Run dtk on a trusted, isolated host.
- Consider using a Docker socket proxy (e.g. [Tecnativa/docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy)) and restrict allowed API calls to:
  - `GET /containers/*`
  - `POST /containers/*/pause`
  - `POST /containers/*/unpause`
  - `POST /containers/*/restart`

## Reporting a vulnerability

To report a security vulnerability, please open a [GitHub issue](https://github.com/ylecuyer/dtk/issues) with the label `security`. For sensitive disclosures, contact the maintainer directly via GitHub.
