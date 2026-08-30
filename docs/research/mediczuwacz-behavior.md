# MediCzuwacz 0.8 behavior and failure modes

## Scope and baseline

This inventory describes the observable behavior of MediCzuwacz 0.8 at the current `main` commit, [`04677e581046249ab10f891022fbf386fa5c4acf`](https://github.com/SteveSteve24/MediCzuwacz/commit/04677e581046249ab10f891022fbf386fa5c4acf). The package metadata and README call this version 0.8. The source code at this commit is the compatibility baseline. [Source: package metadata](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/setup.py#L3-L6) and [source: README changelog](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L231-L257).

The program searches for available Medicover appointments. It does not book, cancel, or change an appointment. Automatic booking is only an open feature request. [Source: current command implementation](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L340-L421) and [source: booking request](https://github.com/SteveSteve24/MediCzuwacz/issues/3).

## Command interface

The program requires one of two top-level commands. `find-appointment` searches for slots. `list-filters` lists IDs that the search command needs. Python `argparse` prints usage and stops before login when a command, required argument, integer, or ISO date is not valid. [Source: parser](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L340-L369).

`find-appointment` has this interface:

| Option | Required | Type and default | Current meaning |
| --- | --- | --- | --- |
| `-r`, `--region` | Yes | One integer | `RegionIds` request value. |
| `-s`, `--specialty` | Yes | One or more integers | `SpecialtyIds` request value. Repeated values are collected in one list. |
| `-c`, `--clinic` | No | One integer | `ClinicIds` request value. |
| `-d`, `--doctor` | No | One integer | Adds `DoctorIds`. |
| `-f`, `--date` | No | ISO date; today by default | Search start date. A past value is changed to today. |
| `-e`, `--enddate` | No | ISO date | Inclusive client-side end date. |
| `-l`, `--language` | No | One integer | Adds `DoctorLanguageIds`. Help lists Polish `4`, English `6`, and Ukrainian `60`. |
| `--search-type` | No | String; `"0"` by default | `SlotSearchType`. Known values are `Standard` or `0`, and `DiagnosticProcedure` or `2`. |
| `-n`, `--notification` | No | String | Selects one notifier by its exact lower-case name. |
| `-t`, `--title` | No | String | Optional notification title. |
| `-i`, `--interval` | No | Integer minutes | Repeats the full login and search cycle after each sleep. |

[Source: command arguments](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L344-L355), [source: search request construction](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L232-L261), and [source: documented search types](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L85-L98).

`list-filters` supports `regions`, `specialties`, `doctors`, and `clinics`. Doctor lookup requires one region and one specialty. Clinic lookup requires one region and one or more specialties. The output is one `id - value` line for each result. All filter requests use `SlotSearchType=0`. [Source: filter parser and output](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L357-L367) and [source: filter request and dispatch](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L263-L273).

The program loads a local `.env` file. It then reads `MEDICOVER_USER` and `MEDICOVER_PASS`. If one is empty or absent, it prints a specific error and exits with status 1. [Source: environment loading](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L25-L31) and [source: credential check](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L369-L376).

## Authentication and persistent session data

Each search and filter operation first authenticates against `https://login-online24.medicover.pl`. The implementation uses an OpenID Connect authorization-code flow with PKCE. It identifies the client as `web`, asks for `openid offline_access profile`, uses Polish UI, and sends a fixed application version, a persistent device ID, and the current Unix time in milliseconds. It uses `https://online24.medicover.pl/signin-oidc` as the redirect URI. [Source: authorization parameters](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L148-L165).

The normal login flow has five steps:

1. Start authorization without automatic redirects.
2. Reuse an immediate authorization code when saved cookies still authenticate the session.
3. Otherwise, load the login page, extract `__RequestVerificationToken`, and submit the user name and password.
4. Complete an MFA redirect when it occurs. Then follow one more redirect and extract the authorization code.
5. Exchange the code at `/connect/token`, put the access token in the `Authorization: Bearer` header, and save cookies.

[Source: login and token exchange](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L80-L92) and [source: login sequence](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L164-L214).

For MFA, the program reads the first form and copies all hidden fields. It asks for a code through standard input. It submits the code with `Input.IsTrustedDevice=true`, device name `Chrome`, and button value `confirm`. A redirect status means success. A trusted device can redirect without a prompt. [Source: MFA implementation](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L94-L146) and [source: trusted-device branch](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L195-L205).

The fixed persistence directory is `/data`. For user name `U`, the program stores cookies in `/data/U_cookies` and the device ID in `/data/U_cookies.device_id`. It uses a Mozilla cookie jar and loads cookies even when they are expired or marked for discard. Separate user names therefore select separate cookie and device files. The program stores the password only in process memory, but `.env.example` instructs users to keep it in an environment variable. The code does not set an explicit file mode for either persistent file. [Source: cookie and device storage](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L31-L74), [source: environment example](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/.env.example#L1-L7), and [source: Docker volume instructions](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L14-L47).

## Appointment and filter data flow

The appointment endpoint is `https://api-gateway-online24.medicover.pl/appointments/api/search-appointments/slots`. A request always has one region value, one or more specialty values, optional clinic value, page 1, page size 5000, an ISO start date, a search type, and `VisitType=Center`. Language and doctor values are added only when the caller supplies them. A successful response supplies appointment records in its `items` field. The program does not request more pages. [Source: slot request](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L232-L256).

The program changes a past start date to the local system date. It does not send the end date to Medicover. It removes records after the inclusive end date after the response arrives. It parses `appointmentDate` with Python ISO date-time rules. [Source: date handling](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L232-L261) and [source: rationale for the past-date rule](https://github.com/SteveSteve24/MediCzuwacz/pull/16).

The filter endpoint is `https://api-gateway-online24.medicover.pl/appointments/api/search-appointments/filters`. Region and specialty values are optional at this layer. The CLI omits them for region and specialty lists. It sends both for doctor and clinic lists. [Source: filter request](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L263-L273) and [source: CLI dispatch](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L411-L419).

Diagnostic procedures need a different search type. This was the cause of a confirmed case where the web portal found an examination and MediCzuwacz did not. Version 0.8 accepts either the documented text or number as an unvalidated string. It does not infer a search type from the specialty. [Source: issue report and confirmed workaround](https://github.com/SteveSteve24/MediCzuwacz/issues/9) and [source: merged search-type change](https://github.com/SteveSteve24/MediCzuwacz/pull/15).

## Result display and change detection

The terminal output for each appointment contains the appointment date, clinic name, doctor name, specialty name, and comma-separated doctor languages. Missing fields normally become `N/A`. With no new records, the program prints `No new appointments found.` [Source: terminal formatter](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L317-L337).

The first check treats every returned record as new. In interval mode, each later check compares each complete appointment object with the objects from the immediately previous check. The state exists only in memory. A process restart treats all current appointments as new. An appointment is new again when it was absent from the previous check and then returns. A change to any field in the appointment object also makes it new. [Source: change-detection loop](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L378-L409). The original persistence request also records that separate one-off runs were stateless; the later implementation added only interval mode. [Source: persistence issue](https://github.com/SteveSteve24/MediCzuwacz/issues/6).

When `--interval` has a nonzero integer, the program sleeps for that number of minutes and starts the full cycle again. It creates a new authenticator, loads cookies, logs in, and creates a new finder on every cycle. A zero value means one check. A negative value reaches `time.sleep` and raises an error. The program has no signal-specific shutdown or state-save operation. [Source: main loop](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L378-L421).

## Notifications

The program supports five exact notifier names: `pushbullet`, `pushover`, `telegram`, `xmpp`, and `gotify`. One run can select only one string. When there are new appointments, the program always formats the message first. An absent or unknown notifier value then causes no notification and no error. [Source: notifier selection](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L276-L314) and [source: invocation condition](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L399-L405).

The notification message has one block per appointment. Each block contains date, clinic, doctor, languages, specialty, and a 50-character separator. All records are put in one message. [Source: notification formatter](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L276-L299).

Provider behavior is as follows:

- Pushbullet and Pushover use the `notifiers` package. They pass an optional title. They catch invalid arguments and print unsuccessful provider results. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L7-L35).
- Telegram also uses the `notifiers` package. It prepends the title as an HTML `b` element and sends with HTML parsing. Its provider reads `NOTIFIERS_TELEGRAM_CHAT_ID` and `NOTIFIERS_TELEGRAM_TOKEN` from the environment. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L38-L51) and [source: confirmed token configuration](https://github.com/SteveSteve24/MediCzuwacz/issues/7).
- XMPP reads `NOTIFIERS_XMPP_JID`, `NOTIFIERS_XMPP_PASSWORD`, and `NOTIFIERS_XMPP_RECEIVER`. It connects, authenticates, and sends one message. It prints one generic failure when one operation returns false. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L53-L69).
- Gotify reads `GOTIFY_HOST` and `GOTIFY_TOKEN`. `GOTIFY_PRIORITY` is optional and defaults to 5 after a missing or invalid value. A missing title becomes `medihunter`. It posts JSON to `{host}/message?token={token}`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L71-L96).

## Packaging and operation

The supported project workflow builds a Docker image from Python 3.9 Alpine and runs `python ./mediczuwacz.py` as the entry point. The image installs build tools and Python dependencies. It does not declare a volume. Users must mount a host directory at `/data` to keep trusted-device files. First-run 2FA also requires an interactive terminal. [Source: Dockerfile](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/Dockerfile#L1-L26) and [source: first-run instructions](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L31-L47).

The README describes two scheduling methods. A user can run one container from cron, or keep one container alive with `--interval`. Cron executions do not share the in-memory appointment list. [Source: cron instructions](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L150-L172) and [source: interval instructions](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L200-L208).

The project uses GPL-3.0. [Source: license](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/LICENSE#L1-L20).

## Failure modes and diagnostics

### Authentication

- A missing interactive input stream during first-run MFA prints a Docker `-it` instruction and exits with status 1. An empty code raises `ValueError`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L118-L126). This explicit diagnostic was added after a user got an unhandled `EOFError`. [Source: report and successful `-it` test](https://github.com/SteveSteve24/MediCzuwacz/issues/18) and [source: fix](https://github.com/SteveSteve24/MediCzuwacz/pull/21).
- An MFA page error in a `div.alert-error` becomes a printed `MFA error` and a `ValueError`. This includes server messages such as a one-time-code rate limit. A missing form also becomes a `ValueError`. A verification response that is not a redirect prints matching error elements and raises `ValueError`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L94-L143) and [source: stated rate-limit behavior](https://github.com/SteveSteve24/MediCzuwacz/pull/20).
- The code assumes specific redirects and response fields. A missing login `Location`, missing final `code`, non-JSON token response, or missing `access_token` can cause an unhandled exception. HTTP calls have no explicit timeout and no application retry. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L80-L92) and [source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L164-L214).
- Invalid credentials have produced a missing redirect and a malformed host error in an old login flow. A Medicover portal change also caused a missing CSRF element until the client version and timestamp changed. These reports show that the scraped login flow changes over time. [Source: invalid-credential report](https://github.com/SteveSteve24/MediCzuwacz/issues/4), [source: authentication regression](https://github.com/SteveSteve24/MediCzuwacz/issues/10), and [source: fix commit](https://github.com/SteveSteve24/MediCzuwacz/commit/3cab2c16db915e29c0af90ce4acc010c663df52e).
- Cookie load and save errors only print warnings. The program continues. Device-ID read and write errors are not caught. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L48-L74).

### Search and output

- A Medicover API response with a status other than 200 prints the status and body, then becomes an empty object. Appointment search therefore reports no new appointments after an API error. Filter listing indexes the missing filter key and can raise `KeyError`. [Source: HTTP wrapper](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L222-L230), [source: appointment fallback](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L254-L261), and [source: filter indexing](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L411-L419).
- Network errors, timeouts from lower layers, and invalid JSON are not caught. The requests also have no explicit timeout. These failures stop the process with an exception. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L222-L230).
- If `doctor`, `clinic`, or `specialty` exists with a JSON null value, the formatter calls `.get` on `None`. The open report for a `doctor: null` appointment confirms this crash. It affects both terminal and notification formatting. [Source: formatter code](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L283-L290), [source: terminal code](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L325-L331), and [source: open issue](https://github.com/SteveSteve24/MediCzuwacz/issues/14).
- A missing or invalid `appointmentDate` stops end-date filtering with `KeyError` or `ValueError`. No end-date check means the same record can still display with `N/A`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L254-L261) and [source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L325-L337).

### Notifications and process state

- Provider configuration and delivery errors usually print a message and do not change the process exit status. Pushbullet, Pushover, and Telegram only catch `BadArguments`. XMPP only catches a missing environment key. Other provider exceptions can stop the process. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L11-L69).
- Gotify catches request exceptions, but it does not set a timeout or check the HTTP status. A server rejection can appear successful. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L71-L96).
- Telegram enables HTML parsing but does not escape the user title or appointment data. Provider parsing can fail when data contains HTML control characters. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/medihunter_notifiers.py#L38-L51).
- The program sends all new appointments in one provider message. It does not split a message to meet provider size limits. [Source: message construction and send](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L276-L314).
- A process restart loses the appointment comparison state. This makes every current appointment new again. Cookie persistence does not change this result. [Source: in-memory state](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L378-L397) and [source: persistent data scope](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L31-L74).

### Packaging

- The declared console entry point is `mediczuwacz=mediczuwacz:mediczuwacz`, but the module has no `mediczuwacz` function. The Docker path does not use this entry point. It runs the file directly. [Source: package entry point](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/setup.py#L21-L24), [source: module entry](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L424-L425), and [source: Docker entry point](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/Dockerfile#L23-L26).
- A source update does not update an existing local Docker image. The user must rebuild it. One reported version 0.8 authentication failure was an old image, and rebuilding fixed it. [Source: report and resolution](https://github.com/SteveSteve24/MediCzuwacz/issues/22) and [source: build instruction](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L14-L23).
- The repository has no automated tests or CI workflow at the baseline commit. Authentication, response-schema, notification, and persistence behavior therefore have no executable compatibility checks in the source project. [Source: repository tree](https://github.com/SteveSteve24/MediCzuwacz/tree/04677e581046249ab10f891022fbf386fa5c4acf).

## Compatibility contract for the Go rewrite

A compatibility test set must cover these observable cases:

1. Parse both command groups and all options above. Keep current defaults, accepted multi-specialty values, and ISO date handling.
2. Authenticate with PKCE, active MFA, trusted-device reuse, per-account session data, and clear first-run non-interactive failure.
3. Send the same slot and filter query values, including `PageSize=5000`, `VisitType=Center`, and client-side inclusive end-date filtering.
4. Accept both text and numeric search-type values. Do not infer a diagnostic search from a specialty unless the new specification makes this an intentional change.
5. Model nullable Medicover response fields. The Go rewrite must not copy the confirmed null-doctor crash.
6. Distinguish an empty successful result from authentication, transport, HTTP, and response-format failures. The Python fallback to an empty result is observable, but it is unsafe to preserve.
7. Preserve the five message transports and the appointment fields in message content. Treat invalid notifier names and provider rejection as explicit errors in the new interface.
8. Make state semantics explicit. Version 0.8 compares only with the previous in-process result. Durable deduplication across `check`, cron, Docker restarts, and `watch` is new behavior, not existing compatibility behavior.
9. Put timeouts, cancellation, bounded retry, and graceful shutdown in the new implementation. Version 0.8 has none of these controls.

Items 5 through 9 identify intentional safety changes for the new specification. They must not be mistaken for behavior that version 0.8 already provides.
