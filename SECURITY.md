# Security Policy

## Supported Versions

Security fixes are prioritized on the latest code on `main`.

## Reporting a Vulnerability

Please do **not** disclose vulnerabilities in public issues.

Recommended channels:

- Use this repository's GitHub Security page to submit a private vulnerability report when that feature is available.
- Otherwise, use the maintainer contact methods listed on the maintainer profile page: <https://github.com/keakon>.

Please include:

- Impact and affected component
- Reproduction steps / PoC
- Suggested mitigation (optional)

## Operational Notes

`chord-gateway` can execute local `chord headless` processes and route IM messages.
Treat credentials (Feishu app secret, WeChat token files)
as sensitive data and keep them outside version control.

For runtime security boundaries, access control, and deployment guidance, see [docs/permissions-and-safety.md](./docs/permissions-and-safety.md).
