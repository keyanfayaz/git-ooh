# Security policy

## Supported versions

Ooh is currently pre-1.0. Security fixes are applied to the latest release only.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use GitHub’s **Report a vulnerability** flow in the Security tab of the Ooh repository. Include the Ooh version, operating system, Git version, reproduction steps, and impact. Remove source code, secrets, access tokens, and private repository paths before submitting.

You should receive an acknowledgement within seven days. We will coordinate disclosure and credit with you after a fix is available.

## Security boundaries

Ooh is a local developer tool, not an operating-system sandbox for untrusted agents or code. Each variation uses a separate local clone with copied (non-hardlinked) Git objects and no source remote. Ooh asks Codex and Claude Code to use their workspace-oriented permission modes, but those controls belong to the provider and are not a cross-platform containment guarantee.

Agent, Preview, and terminal processes still run with the operating-system permissions and credentials of the user who launched Ooh. Preview install/run commands are shown for confirmation before their first execution in a repository. The interactive terminal is intentionally unrestricted beyond starting in the selected clone.

The dashboard listens only on loopback and requires a random session token for API requests. Do not expose it through a proxy or port-forwarding service. Artifact uploads accept a small set of file types and are stored locally with private permissions, but artifact content should still be treated as untrusted.
