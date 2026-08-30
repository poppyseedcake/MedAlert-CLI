# Go and Linux implementation options

Research date: 2026-08-30

## Question

Which maintained Go packages and Linux facilities can support these functions?

- A full-screen terminal interface in Polish.
- Commands for automation.
- SQLite storage.
- HTTP login, cookies, and provider calls.
- Gotify, Telegram, Pushbullet, Pushover, and XMPP notifications.
- Structured logs.
- Docker images for Linux `amd64` and `arm64`.
- A `systemd --user` service.

This document gives decision inputs. It does not select the final architecture.

## Evidence rules

This research uses official documentation, standards, package documentation, and upstream source repositories. Version and activity data are a snapshot from the research date. A recent release is a useful maintenance signal, but it is not a support guarantee.

## Platform baseline

Go supports both `linux/amd64` and `linux/arm64` as valid target pairs. The Go toolchain sets the target with `GOOS` and `GOARCH`.[Go target documentation](https://go.dev/doc/install/source#environment)

Most options in this report are Go-only code. They do not add a C toolchain requirement. The important exception is `mattn/go-sqlite3`. Some optional `go-systemd` packages also use CGO. The details are in the applicable sections.

The newest package versions in this report require different Go versions. The current Charm v2 modules and `modernc.org/sqlite` require Go 1.25. The current Mellium XMPP module requires Go 1.24. Cobra v1.10.2 requires Go 1.15, and `urfave/cli` v3.11.0 requires Go 1.22.[Bubble Tea v2.0.9 module](https://github.com/charmbracelet/bubbletea/blob/v2.0.9/go.mod) [Bubbles v2.2.1 module](https://github.com/charmbracelet/bubbles/blob/v2.2.1/go.mod) [Lip Gloss v2.0.6 module](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/go.mod) [modernc.org/sqlite v1.57.0 module](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/go.mod) [Mellium XMPP v0.23.0 module](https://codeberg.org/mellium/xmpp/src/tag/v0.23.0/go.mod) [Cobra v1.10.2 module](https://github.com/spf13/cobra/blob/v1.10.2/go.mod) [urfave/cli v3.11.0 module](https://github.com/urfave/cli/blob/v3.11.0/go.mod)

This version spread makes the minimum Go version a project decision. The decision must use the selected package versions, not only the language features that the application uses.

## Full-screen terminal interface

### Option A: Bubble Tea v2, Bubbles v2, and Lip Gloss v2

Bubble Tea uses a model with `Init`, `Update`, and `View`. Version 2 can request the alternate screen in the returned view. This supports a full-window terminal program.[Bubble Tea repository](https://github.com/charmbracelet/bubbletea) [Bubble Tea v2 release notes](https://github.com/charmbracelet/bubbletea/releases/tag/v2.0.9) [Bubble Tea `Model` documentation](https://pkg.go.dev/charm.land/bubbletea/v2#Model)

Bubbles supplies controls such as text input, lists, tables, and viewports. Lip Gloss supplies layout and style functions. The three modules use the MIT license.[Bubbles repository](https://github.com/charmbracelet/bubbles) [Lip Gloss repository](https://github.com/charmbracelet/lipgloss) [Bubble Tea license](https://github.com/charmbracelet/bubbletea/blob/v2.0.9/LICENSE) [Bubbles license](https://github.com/charmbracelet/bubbles/blob/v2.2.1/LICENSE) [Lip Gloss license](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/LICENSE)

All three projects had current v2 releases in August 2026. Bubble Tea v2.0.9, Bubbles v2.2.1, and Lip Gloss v2.0.6 are current maintenance signals.[Bubble Tea releases](https://github.com/charmbracelet/bubbletea/releases) [Bubbles releases](https://github.com/charmbracelet/bubbles/releases) [Lip Gloss releases](https://github.com/charmbracelet/lipgloss/releases)

The tagged source for these three modules has no CGO import. These packages do not by themselves stop a `CGO_ENABLED=0` build.[Bubble Tea v2.0.9 source](https://github.com/charmbracelet/bubbletea/tree/v2.0.9) [Bubbles v2.2.1 source](https://github.com/charmbracelet/bubbles/tree/v2.2.1) [Lip Gloss v2.0.6 source](https://github.com/charmbracelet/lipgloss/tree/v2.0.6)

Polish labels can be normal Go strings. Lip Gloss measures terminal cell width and handles ANSI text. Polish diacritics do not need a separate localization package. This statement is an inference from the string-based interfaces. The terminal must still have a font and locale that can display the characters.[Lip Gloss repository](https://github.com/charmbracelet/lipgloss) [Lip Gloss source](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/style.go)

The `Update` and `View` methods give a direct unit-test surface. A test can send messages to a model and compare the new state or rendered text. The Charm repository also has an experimental `teatest` package. Its experimental location means that it is not as stable as the main Bubble Tea interface.[Bubble Tea `Model` documentation](https://pkg.go.dev/charm.land/bubbletea/v2#Model) [Charm experimental packages](https://github.com/charmbracelet/x/tree/main/exp/teatest)

This stack gives much control over state and rendering. It also means that the application must define more screen behavior than a widget-first toolkit.

### Option B: tview with tcell

`tview` supplies ready-made forms, tables, lists, modal pages, and flex layouts. It uses `tcell` for terminal input and rendering.[tview repository](https://github.com/rivo/tview) [tview package documentation](https://pkg.go.dev/github.com/rivo/tview)

`tcell` supports UTF-8, grapheme clusters, wide characters, Linux terminals, and a simulated screen for tests. It is pure Go and does not need CGO.[tcell repository](https://github.com/gdamore/tcell) [tcell v2 package documentation](https://pkg.go.dev/github.com/gdamore/tcell/v2)

The `tview` license is MIT. The `tcell` license is Apache-2.0.[tview license](https://github.com/rivo/tview/blob/v0.42.0/LICENSE.txt) [tcell license](https://github.com/gdamore/tcell/blob/v2.13.10/LICENSE)

The latest tagged `tview` release is v0.42.0 from August 2025. The repository still had changes in 2026. The current `tcell/v2` module release is v2.13.10 from May 2026. These are positive but different maintenance signals.[tview v0.42.0 release](https://github.com/rivo/tview/releases/tag/v0.42.0) [tview commits](https://github.com/rivo/tview/commits/master/) [tcell v2.13.10 package](https://pkg.go.dev/github.com/gdamore/tcell/v2@v2.13.10)

`tcell.NewSimulationScreen` permits key injection and screen-content inspection. This makes widget-level terminal tests possible without a real terminal.[tcell simulation documentation](https://pkg.go.dev/github.com/gdamore/tcell/v2#SimulationScreen)

This stack gives more ready-made widgets. Its event and widget model is different from the explicit state-transition model in Bubble Tea.

### TUI decision points

- Decide if explicit application state or ready-made widgets are more important.
- Check resize behavior, focus order, validation errors, password input, and Polish text in a small prototype.
- Test both an UTF-8 terminal and a minimal terminal type.
- Keep program logs away from the screen renderer. Both logging options below can write to a selected `io.Writer`.[`slog` package](https://pkg.go.dev/log/slog) [Zap package](https://pkg.go.dev/go.uber.org/zap)

## Command parsing

### Option A: Cobra

Cobra supports nested commands, flags, help, and shell completion. It uses the Apache-2.0 license. Version v1.10.2 was released in December 2025, and the repository had later activity in 2026.[Cobra repository](https://github.com/spf13/cobra) [Cobra v1.10.2 release](https://github.com/spf13/cobra/releases/tag/v1.10.2) [Cobra license](https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt)

Cobra lets tests replace arguments, input, standard output, and standard error. Its `SetArgs`, `SetIn`, `SetOut`, and `SetErr` methods are explicit test seams.[Cobra command source](https://github.com/spf13/cobra/blob/v1.10.2/command.go)

### Option B: urfave/cli v3

`urfave/cli` supports commands, subcommands, flags, environment values, completion, and generated documentation. Its core has no dependency outside the Go standard library. It uses the MIT license.[urfave/cli repository](https://github.com/urfave/cli) [urfave/cli v3 documentation](https://cli.urfave.org/v3/) [urfave/cli license](https://github.com/urfave/cli/blob/v3.11.0/LICENSE)

Version v3.11.0 was released in August 2026. The v3 action receives a `context.Context`, and the root command can run with a supplied argument slice. These interfaces support cancellation and table-driven command tests.[urfave/cli v3.11.0 release](https://github.com/urfave/cli/releases/tag/v3.11.0) [urfave/cli package documentation](https://pkg.go.dev/github.com/urfave/cli/v3)

### Option C: the standard `flag` package

The standard `flag` package has no external dependency. A `FlagSet` can define an independent group of flags, and it can parse a supplied argument slice. The application must build the command tree, help structure, and completion behavior itself.[Go `flag` package](https://pkg.go.dev/flag)

### CLI decision points

- Define the required command tree before package selection.
- Keep command handlers separate from parser objects. Then the menu and commands can call the same application operations.
- Test exit codes, output streams, cancellation, and non-interactive behavior.
- Do not let the command package become the domain interface. The parser must stay an adapter at the command seam.

## SQLite

Both main options implement Go's `database/sql` driver interface. `database/sql` supports contexts, transactions, prepared statements, and connection-pool controls.[Go `database/sql` package](https://pkg.go.dev/database/sql)

### Option A: modernc.org/sqlite

`modernc.org/sqlite` is a CGO-free port of SQLite. The current documentation lists both `linux/amd64` and `linux/arm64` as supported targets.[modernc.org/sqlite package documentation](https://pkg.go.dev/modernc.org/sqlite)

Version v1.57.0 was published in August 2026. Its main package uses the BSD-3-Clause license. It also bundles public-domain SQLite code. The optional `vec` subpackage has separate MIT and Apache-2.0 notices.[modernc.org/sqlite change log](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/CHANGELOG.md) [modernc.org/sqlite license](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/LICENSE) [bundled SQLite notice](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/LICENSE-SQLITE) [sqlite-vec notice](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/LICENSE-SQLITE_VEC)

The package documentation warns that an application must use the exact `modernc.org/libc` version from the driver's `go.mod`. This is a dependency-pinning constraint.[modernc.org/sqlite package documentation](https://pkg.go.dev/modernc.org/sqlite)

The CGO-free implementation makes Go cross-compilation for both target architectures simpler. It also makes a static container build easier. The package is a large generated port, so application tests must cover migrations, locking, busy handling, and recovery on the selected version.[modernc.org/sqlite package documentation](https://pkg.go.dev/modernc.org/sqlite)

### Option B: mattn/go-sqlite3

`mattn/go-sqlite3` is a widely used `database/sql` driver. It uses the MIT license. Version v1.14.50 was released in August 2026.[go-sqlite3 repository](https://github.com/mattn/go-sqlite3) [go-sqlite3 v1.14.50 release](https://github.com/mattn/go-sqlite3/releases/tag/v1.14.50) [go-sqlite3 license](https://github.com/mattn/go-sqlite3/blob/v1.14.50/LICENSE)

This driver requires `CGO_ENABLED=1` and a C compiler. Cross-compilation can require a target C compiler. A static musl build needs external-linker flags. The upstream README documents these requirements.[go-sqlite3 build documentation](https://github.com/mattn/go-sqlite3#compilation)

This option can use the bundled SQLite source or the system `libsqlite3` build tag. The second form adds a runtime system-library dependency.[go-sqlite3 build documentation](https://github.com/mattn/go-sqlite3#linux)

### SQLite test and build implications

- Run repository tests against a real temporary SQLite database. A mock cannot verify locking, transactions, pragmas, or migrations.
- Test schema upgrades from every released schema version.
- Run the same tests on `linux/amd64` and `linux/arm64`.
- If the build uses `modernc.org/sqlite`, test with `CGO_ENABLED=0`.
- If the build uses `mattn/go-sqlite3`, build and run with the correct C toolchain on each target architecture.
- Limit driver-specific settings to one storage adapter. This keeps a future driver change local.

## HTTP and cookies

The Go standard library already has the required HTTP client features. `http.Client` handles redirects and cookies. `http.Transport` handles HTTP, HTTPS, proxies, connection reuse, and timeouts. Both clients and transports are safe for concurrent use and should be reused.[Go `net/http` package](https://pkg.go.dev/net/http)

The `http.RoundTripper` interface is a small seam for transport tests. The `httptest` package can start HTTP and HTTPS test servers. These facilities permit deterministic tests for redirects, headers, 2FA responses, expired sessions, timeouts, and malformed provider responses.[Go `RoundTripper` documentation](https://pkg.go.dev/net/http#RoundTripper) [Go `httptest` package](https://pkg.go.dev/net/http/httptest)

The standard `cookiejar` is RFC 6265 compliant and keeps data in memory. It does not persist cookies across process restarts. A persistent session therefore needs application storage and explicit restoration, or a separate persistent jar.[Go `cookiejar` package](https://pkg.go.dev/net/http/cookiejar)

The Go example tells users to configure `golang.org/x/net/publicsuffix`. A missing public-suffix implementation can permit unsafe cross-domain cookies.[Go cookie-jar example](https://go.dev/src/net/http/cookiejar/example_test.go) [Go cookie-jar source](https://go.dev/src/net/http/cookiejar/jar.go)

HTTP error handling must inspect the status code and provider response. `http.Client.Do` does not return an error only because the server returns a non-2xx status.[Go `Client.Do` documentation](https://pkg.go.dev/net/http#Client.Do)

The standard HTTP packages are part of Go and use Go's BSD-style license. They do not add CGO. They support both target architectures through the Go toolchain.[Go license](https://go.dev/LICENSE) [Go target documentation](https://go.dev/doc/install/source#environment)

## Notification adapters

### Common HTTP approach

Gotify, Telegram, Pushbullet, and Pushover all provide HTTP interfaces. A small adapter can use `net/http` directly for each provider. This avoids a separate SDK release cycle and keeps one transport test seam.[Gotify push documentation](https://gotify.net/docs/pushmsg) [Telegram Bot API](https://core.telegram.org/bots/api) [Pushbullet API](https://docs.pushbullet.com/v2/) [Pushover Message API](https://pushover.net/api)

Each adapter must accept a caller-supplied HTTP client or `RoundTripper`. Tests can then use `httptest.Server` and can check the exact method, path, headers, body, timeout, and response parsing.[Go `RoundTripper` documentation](https://pkg.go.dev/net/http#RoundTripper) [Go `httptest` package](https://pkg.go.dev/net/http/httptest)

### Gotify

Gotify accepts an HTTP `POST` to `/message`. It accepts an application token in `X-Gotify-Key`. The required field is `message`; `title`, `priority`, and `extras` are optional.[Gotify push documentation](https://gotify.net/docs/pushmsg) [Gotify API documentation](https://gotify.net/api-docs)

Gotify has an official Go client with an MIT license. Its last tagged release is v2.0.4 from 2019, and its repository had no later code change after May 2020. This is a weak maintenance signal when compared with direct use of the current REST documentation.[Gotify Go client repository](https://github.com/gotify/go-api-client) [Gotify Go client releases](https://github.com/gotify/go-api-client/releases)

### Telegram

The Telegram Bot API is an HTTP interface. `sendMessage` needs a bot token, a target `chat_id`, and message text. The API returns a JSON response with an `ok` field and can include retry information.[Telegram Bot API](https://core.telegram.org/bots/api#sendmessage) [Telegram response format](https://core.telegram.org/bots/api#making-requests)

The bot token is part of the API URL. The HTTP adapter and logger must redact full request URLs and authorization data. This is an inference from Telegram's documented request format.[Telegram Bot API](https://core.telegram.org/bots/api#making-requests)

### Pushbullet

Pushbullet accepts a JSON `POST` to `/v2/pushes`. A note push uses `type`, `title`, and `body`. The API accepts an access token through HTTP authentication.[Pushbullet API](https://docs.pushbullet.com/v2/#pushes) [Pushbullet authentication](https://docs.pushbullet.com/v2/#authentication)

The official API documentation remains the direct contract. The documentation has an old public change history, so contract tests and clear error output are important maintenance controls.[Pushbullet API and change log](https://docs.pushbullet.com/v2/)

### Pushover

Pushover accepts an HTTPS `POST` to `/1/messages.json`. The required fields are the application token, user or group key, and message. It documents success, invalid-request responses, and rate-limit headers.[Pushover Message API](https://pushover.net/api)

Pushover tells clients not to repeat the same request after a 4xx response. The adapter must therefore separate permanent provider errors from retryable transport or server errors.[Pushover response guidance](https://pushover.net/api#friendly)

### XMPP

XMPP is not a simple request-response HTTP provider. RFC 6120 defines TCP connection setup, TLS, SASL authentication, resource binding, and XML stanza exchange. This protocol scope supports the use of a dedicated XMPP package.[RFC 6120](https://www.rfc-editor.org/rfc/rfc6120)

`mellium.im/xmpp` supplies session setup, feature negotiation, events, and XMPP subpackages. Version v0.23.0 was published in May 2026. The package uses the BSD-2-Clause license.[Mellium XMPP package](https://pkg.go.dev/mellium.im/xmpp) [Mellium XMPP v0.23.0 source](https://codeberg.org/mellium/xmpp/src/tag/v0.23.0) [Mellium XMPP license](https://codeberg.org/mellium/xmpp/src/tag/v0.23.0/LICENSE)

The v0 major version means that semantic-version stability is not guaranteed. The module requires Go 1.24. Its tagged source and dependencies do not require CGO.[Mellium XMPP v0.23.0 module](https://codeberg.org/mellium/xmpp/src/tag/v0.23.0/go.mod) [Go module version guidance](https://go.dev/doc/modules/version-numbers)

XEP-0198 adds stanza acknowledgements and stream resumption. These features can improve delivery evidence after a connection failure, but they add state and interoperability work. Their use is a later notifier reliability decision.[XEP-0198](https://xmpp.org/extensions/xep-0198.html)

XMPP adapter tests need a fake sender for normal application tests and a local protocol peer for integration tests. The application notifier interface must not expose XMPP session details.

## Structured logging

### Option A: standard `log/slog`

Go added `log/slog` in Go 1.21. It supports levels, attributes, groups, context-aware calls, text output, JSON output, and a `Handler` interface. It adds no external dependency and uses the Go BSD-style license.[Go structured logging article](https://go.dev/blog/slog) [`slog` package](https://pkg.go.dev/log/slog) [Go license](https://go.dev/LICENSE)

The built-in handlers write to an `io.Writer`. Tests can use a buffer. The standard `testing/slogtest` package verifies custom handler behavior.[`slog` handlers](https://pkg.go.dev/log/slog#Handler) [`slogtest` package](https://pkg.go.dev/testing/slogtest)

### Option B: Zap

Zap is a stable structured logger with JSON output and a lower-level `Core` interface. Version v1.28.0 was released in April 2026. It uses the MIT license.[Zap repository](https://github.com/uber-go/zap) [Zap v1.28.0 release](https://github.com/uber-go/zap/releases/tag/v1.28.0) [Zap license](https://github.com/uber-go/zap/blob/v1.28.0/LICENSE)

The `zaptest/observer` package stores structured entries in memory for tests. Zap adds an external package and more logging-specific types than `slog`.[Zap observer package](https://pkg.go.dev/go.uber.org/zap/zaptest/observer) [Zap package](https://pkg.go.dev/go.uber.org/zap)

### Logging constraints

- Logs must never contain passwords, provider tokens, session cookies, 2FA codes, or Telegram request URLs.
- Interactive mode must not write background log lines into the TUI screen.
- Command and Docker mode can write structured logs to standard error.
- A systemd service can write to standard output and standard error. The journal collects service output by default.[systemd service standard output](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#StandardOutput=)
- Tests must check secret redaction and stable event keys.

## Docker multi-architecture builds

Docker Buildx can build one image index for `linux/amd64` and `linux/arm64`. Docker documents the `--platform` option and the `BUILDPLATFORM`, `TARGETOS`, and `TARGETARCH` build arguments.[Docker multi-platform builds](https://docs.docker.com/build/building/multi-platform/) [Docker build variables](https://docs.docker.com/build/building/variables/#multi-platform-build-arguments)

A multi-stage Dockerfile can compile on the build platform for each target. This avoids target emulation when all dependencies support Go cross-compilation.[Docker cross-compilation guidance](https://docs.docker.com/build/building/multi-platform/#cross-compilation)

A CGO-free dependency set supports direct builds with `CGO_ENABLED=0`, `GOOS=linux`, and the applicable `GOARCH`. A `go-sqlite3` build needs a C toolchain for each target and can need a libc-specific runtime image.[go-sqlite3 cross-compilation documentation](https://github.com/mattn/go-sqlite3#cross-compile)

The application needs trusted root certificates for Medicover and notification HTTPS calls. The Distroless `static` image includes CA certificates and is intended for static Go programs. The Distroless `base` image adds glibc for CGO programs.[Distroless static and base contents](https://github.com/GoogleContainerTools/distroless/blob/main/base/README.md)

The application also needs Polish time-zone behavior. A minimal image must supply time-zone data, or the binary can import `time/tzdata`. That package embeds the IANA database and adds about 450 KB.[Go `time/tzdata` package](https://pkg.go.dev/time/tzdata)

Tests for each image must check HTTPS, DNS, SQLite writes on a mounted volume, signals, time zone, and the non-interactive `watch` command. Build tests should also use `ldd` or an equivalent inspection to detect an unexpected dynamic dependency.

The full-screen menu needs a TTY. The Docker default must remain the non-interactive command path. An operator can still use the menu with an allocated terminal.

## `systemd --user`

A user unit can run the foreground `watch` command. `systemctl --user` talks to the calling user's service manager. Enabling and starting are separate actions, and `--now` can combine them.[systemctl documentation](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html)

User unit search paths include user configuration directories. The exact paths and precedence come from `systemd.unit` and the XDG base-directory rules. The program must not assume that one hard-coded home path is correct.[systemd unit search path](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html#User%20Unit%20Search%20Path)

By default, a user manager can stop after the last login session ends. `loginctl enable-linger` starts the user manager at boot and keeps it after logout. Linger changes machine state and can require administrator policy approval.[loginctl linger documentation](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html#enable-linger%20USER%E2%80%A6)

### Option A: call `systemctl --user`

The program can call the installed `systemctl` command for enable, disable, start, stop, restart, status, and daemon reload. This uses the same command interface that operators know.[systemctl documentation](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html)

Tests need an injected command runner. They must not change the real user manager. Integration tests can run in a disposable Linux environment with systemd.

### Option B: use `go-systemd` D-Bus packages

`github.com/coreos/go-systemd/v22/dbus` can connect to the systemd user instance. It has context-aware calls for unit-file changes, reload, start, stop, and property reads. Version v22.7.0 was released in January 2026, and the repository had later activity in 2026. It uses the Apache-2.0 license.[go-systemd D-Bus package](https://pkg.go.dev/github.com/coreos/go-systemd/v22/dbus) [go-systemd v22.7.0 release](https://github.com/coreos/go-systemd/releases/tag/v22.7.0) [go-systemd license](https://github.com/coreos/go-systemd/blob/v22.7.0/LICENSE)

The `daemon` subpackage implements `sd_notify` and watchdog messages. A service can use it with `Type=notify`, but a simple foreground process can also use `Type=exec` or `Type=simple` without this package.[go-systemd daemon package](https://pkg.go.dev/github.com/coreos/go-systemd/v22/daemon) [systemd service types](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#Type=)

The `dbus` and `daemon` packages do not require the CGO-based journal reader. The separate `sdjournal` package wraps the native journal library and requires CGO plus journal headers. Reading the journal in-process would therefore change static-build constraints.[go-systemd repository](https://github.com/coreos/go-systemd#readme)

### Unit hardening inputs

Systemd provides settings such as restart policy, environment files, file-system protection, private temporary directories, and capability restrictions. The exact hardening set must follow the final file and network needs.[systemd service settings](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html) [systemd execution settings](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)

The service must receive secrets without placing them in the unit file or command line. `systemd` supports credentials and environment mechanisms, but their availability depends on the installed systemd version. This needs a separate compatibility decision.[systemd credentials](https://systemd.io/CREDENTIALS/) [systemd `LoadCredential`](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#LoadCredential=ID:PATH)

## License summary

| Option | License |
| --- | --- |
| Go standard library | BSD-style |
| Bubble Tea, Bubbles, Lip Gloss | MIT |
| tview | MIT |
| tcell | Apache-2.0 |
| Cobra | Apache-2.0 |
| urfave/cli | MIT |
| modernc.org/sqlite | BSD-3-Clause, with bundled notices |
| mattn/go-sqlite3 | MIT |
| Mellium XMPP | BSD-2-Clause |
| Zap | MIT |
| go-systemd | Apache-2.0 |

The upstream license files support this table.[Go license](https://go.dev/LICENSE) [Bubble Tea license](https://github.com/charmbracelet/bubbletea/blob/v2.0.9/LICENSE) [tview license](https://github.com/rivo/tview/blob/v0.42.0/LICENSE.txt) [tcell license](https://github.com/gdamore/tcell/blob/v2.13.10/LICENSE) [Cobra license](https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt) [urfave/cli license](https://github.com/urfave/cli/blob/v3.11.0/LICENSE) [modernc.org/sqlite license](https://gitlab.com/cznic/sqlite/-/blob/v1.57.0/LICENSE) [go-sqlite3 license](https://github.com/mattn/go-sqlite3/blob/v1.14.50/LICENSE) [Mellium XMPP license](https://codeberg.org/mellium/xmpp/src/tag/v0.23.0/LICENSE) [Zap license](https://github.com/uber-go/zap/blob/v1.28.0/LICENSE) [go-systemd license](https://github.com/coreos/go-systemd/blob/v22.7.0/LICENSE)

The GNU Project lists Apache-2.0 as compatible with GPLv3. It also lists common permissive licenses as GPL-compatible. Distribution must still include the applicable license notices.[GNU license list](https://www.gnu.org/philosophy/license-list.html#GPLCompatibleLicenses) [GNU license compatibility guide](https://www.gnu.org/licenses/license-compatibility.html)

## Testability summary

| Area | Useful seam | Test facility |
| --- | --- | --- |
| TUI state | Model or screen adapter | Direct state tests, rendered-text tests, or `tcell` simulation |
| Commands | Command runner functions | Supplied args and in-memory input/output |
| SQLite | Storage interface | Real temporary database and migration fixtures |
| Medicover HTTP | Supplied HTTP client | `RoundTripper` fake and `httptest.Server` |
| HTTP notifiers | Supplied HTTP client | Provider contract fixtures and error cases |
| XMPP | Small sender interface | Fake sender and local protocol integration peer |
| Logs | Supplied logger or handler | Buffer, `slogtest`, or Zap observer |
| systemd | Command or manager interface | Fake executor or fake manager; disposable systemd integration test |
| Time | Supplied clock | Fixed time and DST cases |

These seams keep the menu, command parser, storage driver, network clients, and Linux manager as adapters. The application behavior can then be tested without a terminal, external provider, or real user service manager.

## Decision tickets that this research enables

1. Select the TUI interaction model after a small Polish-text and resize prototype.
2. Select the command parser after the command tree is specified.
3. Select the SQLite driver together with the static-build and minimum-Go-version policy.
4. Specify cookie persistence and session restoration in the storage model.
5. Specify one notifier result model for permanent failure, retryable failure, and accepted delivery.
6. Decide whether XMPP needs stream management and delivery evidence.
7. Select the structured logger and define the secret-redaction rules.
8. Select the runtime container base and the CA and time-zone data policy.
9. Select `systemctl --user` or D-Bus control and define the supported systemd version range.

No item above is a final architecture decision.
