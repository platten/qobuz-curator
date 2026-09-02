# Security policy and design

## Reporting a vulnerability

Do not disclose vulnerabilities, tokens, configuration files, playlist data,
or exploit details in a public issue. Use the repository's **Security** tab to
open a private vulnerability report. Include affected versions, impact,
reproduction steps, and any suggested remediation. If private reporting is not
enabled, ask the maintainer to enable GitHub private vulnerability reporting
without including sensitive details.

## Supported versions

Security fixes are applied to the latest release. This is a personal-use
application built around an undocumented Qobuz web API; older releases are not
supported when that upstream interface changes.

## Security boundary

Qobuz Curator is a single-user, local-first application. Its supported default
is one unprivileged process bound to loopback. The browser and local operator
are trusted. Uploaded JSON, form input, OpenAI responses, Qobuz responses, HTTP
headers, and playlist metadata are untrusted.

The application does not provide TLS termination, multi-user authorization,
at-rest encryption, or a public-internet security boundary. Use a trusted HTTPS
reverse proxy or Tailscale for remote access and an encrypted filesystem when
playlist history or tokens are sensitive.

## Implemented safeguards

### Process and filesystem

- Startup is refused for Unix UID 0 and for an elevated Windows token. No
  command requires root or Administrator privileges.
- Kirsle's `configdir` chooses a deterministic per-user configuration path;
  the current directory and system-wide directories are not searched.
- `qobuz-curator init` creates configuration with a unique random session
  secret and user-only permissions. Interactive secret input is not echoed on
  a terminal, and an existing file requires explicit `--force` replacement.
- Credential updates use a private temporary file, flush it, and atomically
  replace the target. Configuration and SQLite files use user-only permissions
  where the operating system supports them.
- SQLite uses one connection, WAL mode, a busy timeout, and atomic transactions
  for the operation record plus rollback backup.
- The container uses an unprivileged numeric UID/GID, a scratch image, a
  read-only root filesystem, `no-new-privileges`, no Linux capabilities, and a
  small `noexec,nosuid` temporary filesystem.

### Listener and web application

- Native startup binds port `0` by default. The operating system reserves an
  available high port in the same operation that creates the listener, avoiding
  a port-probe race. A fixed `--port` remains available.
- The default host is loopback. Host-header allowlisting mitigates DNS rebinding;
  proxy hostnames must be listed explicitly in `allowed_hosts`.
- Request and upload sizes, HTTP headers, API responses, prompt sizes, source
  playlist counts, source track counts, and metadata serialization are bounded.
- Dynamic responses use `Cache-Control: no-store`. Responses set a restrictive
  Content Security Policy, deny framing and MIME sniffing, suppress referrers,
  and disable camera, location, and microphone permissions.
- Browser state-changing routes require POST, a signed session, a constant-time
  CSRF check, and explicit confirmation for playlist writes.
- Panics are recovered at the HTTP boundary, recorded with a stack, and returned
  as generic errors without internal details.

### Authentication and sessions

- Passwords use bounded scrypt parameters and constant-time verification.
- Password verification is serialized to prevent concurrent guesses from
  multiplying the memory-hard KDF's resource consumption.
- Login failures are rate-limited per direct peer; the limiter and active-session
  registry have hard size bounds.
- Sessions are HMAC-signed, expire, use `HttpOnly` and `SameSite=Strict`, and are
  tracked in process so logout revokes the current session. Use
  `secure_cookies: true` only when every browser path uses HTTPS.
- The Qobuz helper never accepts a Qobuz password. Its callback binds only to
  `127.0.0.1`, uses a cryptographically random callback path, and has bounded
  timeouts.

### Desktop credential storage

- The desktop application stores Qobuz and OpenAI credentials in macOS
  Keychain, Windows Credential Manager, or the Linux Secret Service rather than
  its YAML configuration.
- There is no plaintext fallback. If the platform credential service is locked,
  missing, or unavailable, setup fails closed and displays recovery guidance in
  the graphical window.
- Desktop YAML contains only non-secret settings, is replaced atomically, and
  uses user-only permissions where supported.
- First-run migration copies credentials from the existing CLI configuration to
  the platform vault without modifying or deleting that original file.
- The Wails application delegates directly to the existing HTTP handler and
  opens no public application listener. Only the randomized loopback Qobuz OAuth
  callback remains network-bound during authorization.

### External APIs, privacy, and retries

- Qobuz and OpenAI bearer credentials are sent only in request headers. They are
  never written to playlist JSON, SQLite records, or logs.
- OpenAI Responses requests set `store: false` and require strict structured
  output. Generated data still passes local validation and Qobuz matching.
- Structured logs omit prompts, form bodies, query strings, tokens, cookies,
  authorization codes, and API response bodies. JSON logging is available for
  collectors; console color can be disabled.
- Retry attempts are bounded, context-cancellable, exponentially backed off,
  and jittered. Valid `Retry-After` and OpenAI `X-Should-Retry` instructions are
  honored with a maximum delay.
- Only operations safe to repeat are retried. Qobuz playlist writes and the
  single-use OAuth code exchange are never blindly retried after ambiguous
  network failures.

### Playlist integrity and concurrency

- Previews are immutable and expire. Publication uses the exact reviewed track
  IDs rather than recomputing matches.
- All remote playlist mutations and restores are serialized in process, so two
  requests cannot interleave a backup, clear, write, or verification sequence.
- Append and replace refuse to start unless a complete remote snapshot and the
  pending audit record are committed atomically.
- Every mutation reads the playlist back and verifies exact track order. A
  failure triggers immediate rollback; rollback failure is recorded as a
  critical event and remains visible in the operation audit record.
- Qobuz pagination or metadata that indicates an incomplete backup causes the
  operation to fail closed.

### Software supply chain

- GitHub Actions and Go dependencies are version-pinned; third-party Actions
  use immutable commit SHAs.
- CI enforces formatting, `go vet`, Staticcheck, race-enabled tests, at least 90%
  coverage, and `govulncheck` before publication.
- Every CI run has an independent job that generates and uploads a CycloneDX
  JSON SBOM and SHA-256 manifest with cross-platform binaries. Published containers include BuildKit
  provenance and an image SBOM.
- Production builds disable CGO, strip host paths with `-trimpath`, and disable
  VCS stamping beyond the explicitly embedded version.

## Deployment checklist

For access beyond the local machine:

1. Terminate HTTPS at a trusted reverse proxy or Tailscale endpoint; never expose
   the listener directly to the public internet.
2. Generate a password with `qobuz-curator password-hash` and set
   `auth_disabled: false`.
3. Initialize with `qobuz-curator init`; it generates an independent random
   `session_secret` of at least 32 characters.
4. Set `secure_cookies: true` when every route to the service is HTTPS.
5. Add only the exact proxy DNS name or IP to `allowed_hosts`.
6. Inject Qobuz and OpenAI tokens as secrets and protect the configuration and
   data directory with account-level permissions and encrypted storage.
7. Apply request limiting at the reverse proxy and retain independent backups of
   important playlists.
8. Verify downloaded release files against `SHA256SUMS` and inspect the SBOM when
   required by your environment.

## Incident response and residual risks

If a credential may have leaked, rotate or revoke the Qobuz token, OpenAI API
key, web password hash, and session secret as applicable, then restart the
service to invalidate in-memory sessions. Review structured logs and operation
records, but remember that logs intentionally do not contain request bodies.

Configuration and SQLite data are not encrypted by the application. The Qobuz
consumer API is undocumented and can change without notice. No client can make
that interface stable or make every remote failure transactional. Test new
releases with a disposable playlist and retain independent backups of playlists
you cannot recreate.
