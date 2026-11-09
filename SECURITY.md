# Security policy

## Reporting a vulnerability

Please report a vulnerability in the Uffici companion (`uffici`) privately, through GitHub: open
this repository's **Security** tab and choose **Report a vulnerability**
(<https://github.com/uffici-ai/uffici-companion/security/advisories/new>). Do not open a public
issue, and do not include a real agent key, pairing code or customer data in the report.

If you are a Uffici customer, you can also report it through your account at
[uffici.ai](https://uffici.ai).

Please include what you found, the version (`uffici --version`), the steps to reproduce it, and
what an attacker could do with it. We will acknowledge the report, keep you informed while we
fix it, and credit you in the advisory unless you prefer otherwise.

## Supported versions

Fixes are released as a new version. The release your Uffici agents gateway names, and the
latest release, are supported; upgrade with `uffici upgrade`.

## What is in scope

This repository's code and its releases: the binaries, the signed release assets, the container
image and the install script. The Uffici service itself is out of scope here; report a problem
with it through your account.
