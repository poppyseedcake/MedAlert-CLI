# Medicover authentication and appointment protocol

Research date: 2026-08-30

MediCzuwacz source snapshot: [`04677e581046249ab10f891022fbf386fa5c4acf`](https://github.com/SteveSteve24/MediCzuwacz/commit/04677e581046249ab10f891022fbf386fa5c4acf)

## Scope and safety

This report describes the protocol that MediCzuwacz 0.8 uses. It also checks the public Medicover configuration and public web application. The check used no real account, password, session, or 2FA code. It did not search account data and did not book an appointment.

The report uses these evidence levels:

- **Verified by Medicover**: a public Medicover endpoint or the current Medicover web application confirms the fact.
- **Implemented by MediCzuwacz**: the source code contains the behavior, but this research did not run it with an account.
- **Reported by project users**: a source repository issue or pull request reports the behavior.
- **Not verified**: no available primary source confirms the assumption.

## Decision summary

The Go port can keep the same basic protocol: OIDC authorization code flow with PKCE, an HTML login and MFA flow, a cookie jar for trusted-device state, a bearer access token, and JSON appointment endpoints. However, it must not copy the Python flow line for line.

The current Medicover web application uses `appointments/api/v2`. MediCzuwacz uses `appointments/api` without `v2`. The portal also sends a newer application version than MediCzuwacz. The login and MFA steps use private HTML forms, not a stable public interface. These parts can change without notice.

The Go port must put all Medicover-specific HTTP behavior behind one deep module. The module must validate OIDC state, use current server metadata, preserve cookies safely, accept changed redirect chains, and return typed errors. Contract tests must cover saved-session login, full login, MFA, token exchange, filter discovery, slot search, unauthorized responses, rate limits, and changed response fields.

## Verified OIDC facts

Medicover publishes an [OpenID Connect discovery document](https://login-online24.medicover.pl/.well-known/openid-configuration). On the research date, it declared these facts:

- The issuer is `https://login-online24.medicover.pl`.
- The authorization endpoint is `/connect/authorize`.
- The token endpoint is `/connect/token`.
- The server supports the authorization code and refresh token grants.
- The server supports the `query` response mode.
- The server supports PKCE methods `plain` and `S256`.
- The declared scopes include `openid`, `profile`, and `offline_access`.

The current public Medicover application defines the OIDC client as `web`. It uses `/signin-oidc`, the authorization code response type, the query response mode, and the scopes `openid offline_access profile`. It stores the OIDC user in browser local storage and disables automatic silent renewal. See the current [Medicover application bundle](https://online24.medicover.pl/assets/index-DwH6Lp-v.js) and [environment configuration](https://online24.medicover.pl/env-config.js?3.37.0-beta.1.6).

The same application bundle adds `ui_locales`, `app_version`, `previous_app_version`, `device_id`, `device_name`, and `ts` to the authorization request. The public environment configuration reported application version `3.37.0-beta.1.6` on the research date. These are Medicover extensions to the normal OIDC request.

An unauthenticated request to the official authorization endpoint, with the registered `web` redirect URI and PKCE S256, returned a redirect to `/Account/Login`. The returned login form used these field names:

- `Input.ReturnUrl`
- `Input.Username`
- `Input.Password`
- `__RequestVerificationToken`

The form action included the OIDC callback request. A second public probe with only the normal OIDC parameters also reached the login form. Thus, the extra version and device parameters are not necessary to open the login page. This test does not prove that login, MFA, or trusted-device handling works without them. See the official [authorization endpoint](https://login-online24.medicover.pl/connect/authorize).

An unauthenticated token request for client `web`, with a false code and no client secret, returned `invalid_grant`. This result is consistent with a public PKCE client, but it does not prove a successful exchange. The discovery document lists only secret-based token authentication methods. This metadata mismatch must remain an explicit protocol risk. See the official [token endpoint](https://login-online24.medicover.pl/connect/token).

## MediCzuwacz authentication flow

The following steps are **implemented by MediCzuwacz**. Only the public entry steps were verified without an account.

1. The program creates a 32-character `state`, a persistent UUID device ID, and a 96-character PKCE verifier. It calculates the S256 challenge. It then sends the OIDC parameters and the Medicover extension parameters to `/connect/authorize`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L148-L166)
2. If the first redirect contains `code=`, the program treats saved cookies as a valid session and exchanges that code. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L168-L173)
3. Otherwise, it gets the login page, reads `__RequestVerificationToken`, and posts the user name, password, return URL, login type, and button value. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L175-L194)
4. If the next redirect contains `/Mfa`, it gets the MFA page. If that request redirects, it treats the device as trusted. If it returns an HTML page, it copies every hidden input, asks for a code, and adds `Input.MfaCode`, `Input.IsTrustedDevice=true`, `Input.DeviceName=Chrome`, and `Input.Button=confirm`. It posts the form and requires a redirect response. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L94-L146) [Call site](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L195-L205)
5. It follows one more redirect, reads `code` from the next location, and posts the code, verifier, redirect URI, grant type, and client ID to `/connect/token`. It uses `access_token` as a bearer token. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L207-L214) [Token exchange](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L80-L92)

MediCzuwacz keeps a Mozilla cookie jar and one device UUID per user name under `/data`. It loads cookies even if they are expired and saves session cookies even if they have a discard flag. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L31-L74)

The source repository states that a first interactive MFA run can mark the device as trusted and that later runs can use the saved cookies. [README](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/README.md#L31-L47) A project user also reported that the 2FA flow worked after the container received an interactive terminal. This is user-reported evidence, not an independent Medicover contract. [Issue report](https://github.com/SteveSteve24/MediCzuwacz/issues/18#issuecomment-4184349799)

## Appointment and filter protocol

### Current official web application

The current public Medicover bundle uses the base path `appointments/api/v2`. It defines these authenticated GET operations. See the [Medicover application bundle](https://online24.medicover.pl/assets/index-DwH6Lp-v.js) and the official [API base configuration](https://online24.medicover.pl/env-config.js?3.37.0-beta.1.6).

- Initial filters: `/appointments/api/v2/search-appointments/filters/initial-filters`
- Dependent filters: `/appointments/api/v2/search-appointments/filters`
- Slots: `/appointments/api/v2/search-appointments/slots`

The current filter request can send `RegionIds`, `ClinicIds`, `SpecialtyIds`, `DoctorIds`, `DoctorLanguageIds`, `SelectedSpecialtyIds`, `SlotSearchType`, and `RecommendedVisitId`.

The current slot request can send `RegionIds`, `ClinicIds`, `SpecialtyIds`, `DoctorIds`, `DoctorLanguageIds`, `StartTime`, `EndTime`, `Page`, `PageSize`, `SlotSearchType`, `isOverbookingSearchDisabled`, and `RecommendedVisitId`. The current browser code uses a default page size of 5000. Its visit variant enum contains `Standard` and `DiagnosticProcedure`.

Unauthenticated GET requests to the v2 initial-filter, filter, and slot URLs returned HTTP 401 with an empty response on the research date. This confirms that the operations need authorization. It does not confirm their authenticated response schema. See the official [initial-filter endpoint](https://api-gateway-online24.medicover.pl/appointments/api/v2/search-appointments/filters/initial-filters), [filter endpoint](https://api-gateway-online24.medicover.pl/appointments/api/v2/search-appointments/filters?SlotSearchType=Standard), and [slot endpoint](https://api-gateway-online24.medicover.pl/appointments/api/v2/search-appointments/slots?RegionIds=204&SpecialtyIds=132&StartTime=2026-08-30&Page=1&PageSize=1&SlotSearchType=Standard).

### MediCzuwacz implementation

MediCzuwacz uses the older base path `appointments/api`, without `v2`.

Its slot request sends:

- required in its command model: `RegionIds` and `SpecialtyIds`;
- always: `Page=1`, `PageSize=5000`, `StartTime`, `SlotSearchType`, and `VisitType=Center`;
- optional: `ClinicIds`, `DoctorIds`, and `DoctorLanguageIds`.

It expects a JSON object with an `items` array. It applies the end date locally to `appointmentDate`; it does not send `EndTime`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L232-L261)

Its filter request sends `SlotSearchType=0`. It can also send region and specialty IDs. It expects top-level arrays named `regions`, `specialties`, `doctors`, and `clinics`. Each displayed filter item must have `id` and `value`. [Request source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L263-L273) [Display source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L411-L419)

For a slot, the program reads `appointmentDate`, `clinic.name`, `doctor.name`, `specialty.name`, and `doctorLanguages[].name`. [Source](https://github.com/SteveSteve24/MediCzuwacz/blob/04677e581046249ab10f891022fbf386fa5c4acf/mediczuwacz.py#L276-L299) A repository issue shows that `doctor` can be `null` for an extra appointment. The current code still calls `.get()` on that null value. [Issue](https://github.com/SteveSteve24/MediCzuwacz/issues/14)

## Fragile and unverified assumptions

| Assumption | Evidence | Risk for the Go port |
| --- | --- | --- |
| The private login form keeps the same path and field names. | The public form confirms the current names, but no public contract defines them. | High. Parse by form semantics, keep all hidden fields, and return a typed protocol-change error. |
| The MFA path always contains `/Mfa`, and one form post completes MFA. | MediCzuwacz implements this rule. No credential-free test can reach the form. | High. Model redirects as a bounded state machine. Do not depend on one exact path or redirect count. |
| `Input.IsTrustedDevice=true` plus a stable `device_id` and cookie jar is sufficient. | MediCzuwacz and a project user report success. Medicover publishes no contract for it. | High. Treat the device ID and cookies as one opaque session record. Allow the server to require MFA again. |
| `appointments/api` remains compatible. | MediCzuwacz uses it, but the current official application uses `appointments/api/v2`. | High. Implement v2 first. Do not add silent v1 fallback without a contract test. |
| Version `3.4.0-beta.1.0` remains valid. | MediCzuwacz hard-codes it. The official environment now reports `3.37.0-beta.1.6`. | High. Do not freeze the old value in the domain model. Keep extension parameters inside the Medicover adapter. |
| A response location that contains `code=` is a valid OIDC response. | MediCzuwacz does not compare returned `state` with sent `state`. | Security risk. Generate state with a cryptographic random source and require an exact state match. Validate `iss` if it is present. |
| One redirect after login or MFA is sufficient. | The Python source follows a fixed sequence and then indexes `code`. Earlier project issues show `KeyError: code` failures when the sequence changed. [Issue](https://github.com/SteveSteve24/MediCzuwacz/issues/18) | High. Follow only same-origin redirects, set a small limit, and stop only at the registered callback. |
| A token response always contains `access_token`. | The code does not check the HTTP status or OAuth error fields. | Medium. Decode success and error schemas separately. |
| `offline_access` is useful in the current implementation. | The code requests it but discards the refresh token and expiry. | Medium. Either manage refresh tokens and expiry or remove this scope after an authenticated test. |
| Expired and discard cookies must be reloaded. | The Python cookie jar ignores both flags. | Security and reliability risk. Follow cookie expiry rules. Store only the cookies needed for this account session. |
| A plain cookie-jar file is sufficient protection for session data. | MediCzuwacz writes the jar under `/data` and does not add encryption or file-mode checks. | Security risk. Limit file permissions and document that a copied session store can give account access. |
| Filter discovery always uses search type `0`. | The official client sends the selected `SlotSearchType`. | Functional risk for diagnostic procedures. Pass the profile search type to both filter and slot requests. |
| `VisitType=Center` is required. | MediCzuwacz sends it. The current official slot request code does not send it. | Medium. Keep it out of the core interface. Confirm it in an authenticated contract test before use. |
| Slot fields are always present and nested objects are non-null. | The code assumes this. The `doctor=null` issue disproves part of it. | High. Use nullable transport fields and map them to safe domain values. |
| Page size 5000 returns the full result. | Both clients use 5000, but neither public source guarantees a maximum or total count. | Medium. Read pagination metadata and fetch more pages when necessary. |

The source history confirms that server changes have already broken the flow. A 2025 fix added `ts` and changed the application version after the CSRF field was not found. [Commit](https://github.com/SteveSteve24/MediCzuwacz/commit/3cab2c16db915e29c0af90ce4acc010c663df52e) The 2026 MFA work then replaced an MFA-gate skip with full MFA and persistent cookies. [Commit](https://github.com/SteveSteve24/MediCzuwacz/commit/9bcfa4c5bac33189791795f8f7e8917d8688f9a8) These changes show that the HTML authentication flow is not stable.

## Required module seam for the Go port

Use one external seam for all Medicover behavior. Callers must not know URLs, HTML field names, cookies, redirect paths, bearer headers, transport JSON, application-version parameters, or v1/v2 differences.

A small interface can expose these operations:

```go
type Medicover interface {
	Login(ctx context.Context, account AccountRef, challenge ChallengeHandler) (Session, error)
	Filters(ctx context.Context, session Session, query FilterQuery) (FilterSet, error)
	Search(ctx context.Context, session Session, query SearchQuery) (SearchResult, error)
}
```

This interface is the test surface. The implementation can contain internal seams for an HTTP transport, cookie store, clock, random source, and challenge handler. Do not expose raw HTTP requests or Medicover JSON to commands, the terminal menu, profiles, or notifications.

The session store must keep cookies and the device ID together for one account. It must not contain the password. The login result must report whether it reused a session, required credentials, or required MFA. The implementation must never log passwords, codes, tokens, cookie values, or complete login URLs that contain authorization data.

## Contract tests needed before release

Use redacted HTTP fixtures from a dedicated test account only if the project later gets explicit permission for that test. Until then, use a local fake server for all states.

1. Public OIDC discovery supports code flow and S256.
2. A valid saved session returns to the registered callback.
3. An expired saved session returns to the login form.
4. Login accepts a changed form action and preserves unknown hidden inputs.
5. MFA accepts a changed form action and preserves unknown hidden inputs.
6. A trusted device can skip the code, but the flow can request a code again.
7. Wrong state, wrong issuer, wrong callback host, and too many redirects stop the flow.
8. Token errors do not expose response secrets.
9. HTTP 401 causes one controlled reauthentication, not an infinite loop.
10. HTTP 429 produces a retryable rate-limit error with server delay data when available.
11. Filters and slots use v2 and the selected search type.
12. Pagination, `doctor=null`, missing arrays, unknown fields, and invalid dates do not crash the process.

## Open facts that need an authorized test

This research did not verify these facts:

- the exact current login success redirect sequence;
- the current MFA HTML fields, action, available channels, resend behavior, and rate-limit response;
- which cookies implement the login session and trusted-device state;
- cookie lifetime and trusted-device lifetime;
- access-token and refresh-token lifetime;
- the authenticated v2 filter and slot response schemas;
- whether `VisitType=Center` changes v2 results;
- whether the old non-v2 endpoints are still supported for all accounts;
- stable slot identity fields for new-slot detection;
- account-package differences in filters, prices, and results.

Do not infer these facts from the old Python implementation. Confirm them with redacted fixtures before the Go implementation is declared compatible.
