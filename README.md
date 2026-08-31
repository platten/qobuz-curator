# Qobuz Curator

Qobuz Curator is a single cross-platform Go executable that turns written or JSON music recommendations into reviewed Qobuz playlists for Roon. The executable embeds the complete web UI and playlist schema; it does not require a language runtime, separate asset directory, or Spotify.

> Qobuz does not document a stable public consumer playlist API. The live provider isolates the private web API used by community clients, but Qobuz changes can still break it. Test with a disposable playlist first.

## Features

- Import strict playlist JSON or generate it with the OpenAI API.
- Analyze one or more existing Qobuz playlists as musical taste references.
- Refine recommendations into new previews.
- Deterministically choose the highest-confidence Qobuz recording, with maximum quality as a tie-breaker.
- Preview every match before creating, appending to, or replacing a playlist.
- Back up append/replace destinations and restore them if needed.
- Embed templates, CSS, and the JSON schema in one binary.
- Show progress while OpenAI and Qobuz requests are in flight.
- Emit structured Zap logs in colored console or JSON format.
- Retry temporary read-only failures with bounded exponential backoff and jitter.
- Run on Windows, Linux, macOS, Docker, and Unraid.

## Build and run

Go 1.27 or newer is required to build:

```bash
go build -trimpath -o qobuz-curator .
./qobuz-curator init --interactive
./qobuz-curator
```

The source fallback version is `v0.3.0`. Release builds replace it with the
exact Git tag through linker flags. Go modules do not declare a package version
inside `go.mod`; the publishable module version is the semantic Git tag.

Running without a subcommand is equivalent to `qobuz-curator serve`. After the listener starts, the default browser opens automatically.
The native default is port `0`: the operating system atomically reserves an
available high port and the CLI prints the resulting URL. This avoids both
common-port collisions and check-before-bind races. Specify `--port` (or `-p`)
when a stable port is required.

```bash
qobuz-curator serve --no-browser
qobuz-curator serve --host 127.0.0.1 --port 49277
qobuz-curator init
qobuz-curator init --interactive
qobuz-curator --config /path/to/qobuz-curator.yaml
qobuz-curator config-path
qobuz-curator version
```

Browser launch failure is only a warning. Containers use `--no-browser`.
The executable refuses to run as Unix root or with an elevated Windows token;
no command needs those privileges.

## Configuration

Run `qobuz-curator init` to create a complete configuration with safe defaults,
or `qobuz-curator init --interactive` to be prompted for each runtime setting.
Secret input is hidden on an interactive terminal, a random session secret is
generated automatically, and the resulting file is restricted to the current
user. Existing files are never replaced unless `--force` is supplied. Use
`--config /path/to/file.yaml` to initialize a non-default location.

If the configuration is missing when the server starts, the CLI prints its
expected location and both initialization commands. `qobuz-curator config-path`
prints the default location directly. Kirsle's `configdir` library selects:

- Windows: `%APPDATA%\qobuz-curator\qobuz-curator.yaml`
- macOS: `~/Library/Application Support/qobuz-curator/qobuz-curator.yaml`
- Linux/Unix: `${XDG_CONFIG_HOME:-~/.config}/qobuz-curator/qobuz-curator.yaml`

Configuration precedence is:

1. CLI flags
2. `QOBUZ_CURATOR_*` environment variables
3. YAML configuration
4. built-in defaults

Without `--config`, only that per-user path is consulted. This deterministic
behavior prevents an unexpected working-directory file from supplying secrets.

Former `.env` values map directly to YAML keys by removing `QOBUZ_CURATOR_` and lowercasing the remainder. For example:

| Former environment variable | YAML key |
|---|---|
| `QOBUZ_CURATOR_PROVIDER` | `provider` |
| `QOBUZ_CURATOR_QOBUZ_APP_ID` | `qobuz_app_id` |
| `QOBUZ_CURATOR_QOBUZ_USER_AUTH_TOKEN` | `qobuz_user_auth_token` |
| `QOBUZ_CURATOR_OPENAI_API_KEY` | `openai_api_key` |
| `QOBUZ_CURATOR_OPENAI_MODEL` | `openai_model` |
| `QOBUZ_CURATOR_AUTH_DISABLED` | `auth_disabled` |
| `QOBUZ_CURATOR_PASSWORD_HASH` | `password_hash` |
| `QOBUZ_CURATOR_SESSION_SECRET` | `session_secret` |
| `QOBUZ_CURATOR_SECURE_COOKIES` | `secure_cookies` |
| `QOBUZ_CURATOR_LOG_LEVEL` | `log_level` |
| `QOBUZ_CURATOR_LOG_FORMAT` | `log_format` |
| `QOBUZ_CURATOR_LOG_COLOR` | `log_color` |

Environment overrides remain supported for Docker and secret injection. Protect files containing tokens with user-only permissions. Generate a compatible login password hash with `qobuz-curator password-hash`.

## Qobuz authentication

The authentication helper is built into the executable and never accepts your Qobuz password:

```bash
qobuz-curator auth
qobuz-curator auth --write-config --config qobuz-curator.yaml
```

The command discovers the web-player app ID, opens official Qobuz browser authorization, receives a randomized localhost callback, verifies the token, and prints copy-ready YAML. `--write-config` is explicit, updates only the two Qobuz credential keys, and never copies environment-provided secrets into the file. See [docs/qobuz-auth.md](docs/qobuz-auth.md).
Without `--config`, `--write-config` writes atomically to the platform-native
path shown by `config-path` with user-only permissions.

Set `provider: qobuz` only after credentials are present.

## OpenAI recommendations

Set `openai_api_key` and optionally `openai_model`. ChatGPT Plus does not include API usage. The application sends written prompts or selected Qobuz playlist metadata to the OpenAI Responses API with a strict structured-output schema. Audio is never sent.

Without an API key, attach [schemas/playlist-v1.schema.json](schemas/playlist-v1.schema.json) to ChatGPT, use [prompts/chatgpt-playlist.md](prompts/chatgpt-playlist.md), and upload the returned JSON on the dashboard.

## Docker and Unraid

```bash
mkdir -p data
cp qobuz-curator.example.yaml data/qobuz-curator.yaml
docker compose up --build -d
```

Compose binds private-range port `49277` to localhost, mounts `./data` for YAML
and SQLite state, overrides the container listener to `0.0.0.0`, runs without
root, and drops Linux capabilities. On Unraid, map `/data` to persistent
appdata and container port `49277` to the desired host port.

The image also uses a read-only root filesystem. Ensure the mapped `/data`
directory is writable by container UID/GID `65532`.

## Secure deployment

The supported default is a local, single-user service bound to loopback. If it
will be reachable from another device or through Tailscale Funnel/reverse
proxy:

1. Terminate HTTPS at the proxy and do not expose the application port directly
   to the internet.
2. Run `qobuz-curator password-hash`, set `auth_disabled: false`, and place the
   resulting value in `password_hash`.
3. Generate an independent secret, for example with `openssl rand -hex 32`, and
   set `session_secret`. Known example secrets are rejected.
4. Set `secure_cookies: true` when every browser connection uses HTTPS.
5. Add the exact external DNS name or IP address to `allowed_hosts` (without a
   port). Keep the list as small as possible.
6. Prefer environment/secret injection for the OpenAI and Qobuz tokens. Protect
   the YAML and SQLite data directory with user-only permissions and encrypted
   storage where appropriate.

Authentication, HTTPS, and `allowed_hosts` are separate controls; all are
needed for an internet-reachable installation. Sessions expire after
`session_ttl_hours`, logout revokes the current session, and an application
restart intentionally requires a new login.

See [SECURITY.md](SECURITY.md) for the security model, credential-rotation
guidance, and private vulnerability reporting process.

## Tests and release builds

```bash
go fmt ./...
go vet ./...
go test -race -covermode=atomic -coverprofile=coverage.out ./internal/...
go tool cover -func=coverage.out
CGO_ENABLED=0 go build -trimpath .
```

Create all production artifacts locally with either shell:

```bash
bash scripts/build.sh
```

```powershell
.\scripts\build.ps1
```

Both scripts fail if any target or SBOM step fails. They build Windows, Linux,
and macOS binaries for AMD64 and ARM64, embed the version, generate a CycloneDX
JSON SBOM from an actual production binary, and write `dist/SHA256SUMS`. Set `VERSION` in Bash or pass
`-Version` in PowerShell to override the Git-derived version.

GitHub Actions enforces formatting, vet, static analysis, vulnerability checks,
race tests, and 95% coverage. Every CI run independently builds and uploads the
same binary, checksum, license, README, and SBOM artifact set, so the SBOM job
does not disappear when a separate test job fails. Successful validation also builds the Linux AMD64
container; eligible default-branch/tag images are published with provenance and
a container SBOM to `ghcr.io/<owner>/<repository>`.

## Logging and failure handling

`log_level` accepts `debug`, `info`, `warn`, or `error`. `log_format` accepts
human-readable `console` or machine-readable `json`; ANSI level colors can be
disabled with `log_color: false`. Use `--color auto|always|never` for friendly
CLI text. Logs include lifecycle, authentication outcome, request status,
preview, publication, rollback, and critical-failure events, but omit bearer
tokens, prompt bodies, form bodies, and URL query strings.

Catalog reads and recommendation calls retry bounded temporary transport errors
and eligible 408/425/429/5xx responses with backoff and jitter. `Retry-After`
and OpenAI's `X-Should-Retry` are honored. Qobuz playlist mutations and OAuth
code exchange are never blindly retried because an ambiguous response could
duplicate a write or consume a one-time code.

## Runtime data and compatibility

The application uses stable SQLite table and JSON payload shapes for previews, operations, and backups. The database is created automatically under `data_dir`. Embedded assets are served directly from memory and are never extracted.

See [ARCHITECTURE.md](ARCHITECTURE.md) for package boundaries, data flows, trust boundaries, and failure handling. See [samples/playlist.json](samples/playlist.json) for an import example.

## License

MIT
