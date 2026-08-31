# Qobuz browser authentication

Run `qobuz-curator auth`. The command downloads the public Qobuz web-player bundle to discover its current application ID, opens the official Qobuz authorization page, and listens temporarily on `127.0.0.1:8765` for the OAuth callback. Your password, CAPTCHA, and multi-factor interaction remain in the browser.
An interactive spinner reports discovery, browser waiting, and token exchange;
redirected output receives stable progress lines instead.

Options:

```text
--app-id ID          skip application-ID discovery
--app-id-only        print the discovered ID and exit
--callback-port PORT localhost callback port; 0 chooses a free port
--timeout SECONDS    browser authorization timeout
--json               print credentials as JSON
--write-config       update the selected YAML file explicitly
```

The user token grants access to your Qobuz account. Do not commit it, paste it into logs, or expose the configuration file through the web server. On Unix, keep the file mode at `0600`.
`--write-config` uses the explicit `--config` path or the per-user path printed
by `qobuz-curator config-path`. The update is written to a private temporary
file, flushed, and atomically renamed so an interrupted write cannot truncate
the previous configuration.

Read-only discovery and profile validation use bounded retries. The one-time
authorization-code exchange is deliberately not retried after an ambiguous
network failure; rerun `auth` to obtain a fresh code instead.
