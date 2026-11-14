# Security policy

## Supported versions

Only the latest release of fogping gets security fixes. Upgrading is a binary swap;
the data directory stays compatible.

## Reporting a vulnerability

Please report privately, not in a public issue: on GitHub open the
repository's **Security** tab and choose **Report a vulnerability**. Include the
version (`fogping --version`), how it is deployed (binary, service or container,
behind a proxy or not), and steps to reproduce.

You should get a first answer within 7 days. Fixed issues are listed in the
release notes of the version that fixes them, with credit if you want it.

## Scope and known design choices

- fogping does not terminate TLS. Put it behind a reverse proxy with TLS, or bind
  it to localhost, when it is reachable beyond a trusted network.
- Every release file has signed build provenance (Sigstore, via GitHub
  artifact attestations) and an SPDX SBOM (`*.sbom.json`). To check that a
  download was built by this repository's release workflow:
  `gh attestation verify <file> -R githubflyideas/fogping`. `SHA256SUMS` lists
  SHA-256 sums for everything, SBOMs included.
- Dependencies and the Go toolchain are checked with `govulncheck` on every
  push and weekly.
- Pass the login password through `FOGPING_PASSWD` or `passwd-file=`, not
  `passwd=`: command-line arguments are visible to every local account.
