# Qobuz Curator Desktop

This directory contains the additive Wails v2.15.0 desktop launcher. It reuses
the same Go application and server-rendered interface as the CLI while keeping
credentials in Keychain, Windows Credential Manager, or Linux Secret Service.

Run development builds from this directory:

```bash
wails dev -tags desktop,webkit2_41
```

Production packaging is performed by the root build scripts and GitHub Actions.
