# Incident policy

The manual check and `watch` use the incident policy in `internal/monitoring`.
The policy classifies failures, selects the incident scope, and saves incident
changes. Command adapters format output and control when notifications are sent.

- Authentication failures belong to the account. Authentication failures during
  a search also belong to the account, not to each profile.
- Local conflicts, stale results, disabled profiles, and cancellation do not
  create profile incidents. Missing secret input does not create an account
  incident.
- Existing store rules still control notification thresholds and duplicate
  prevention. Temporary failures become notifiable after three failures.
- A complete check resolves profile and account incidents. Both scopes are
  attempted. Saved changes and any errors are returned, including when only one
  scope succeeds. The manual check reports a storage error; `watch` logs it and
  reports saved recovery changes.
- A permanent availability delivery failure opens a destination incident.
  Cancelled deliveries and exhausted retries do not open one. Failure wins
  over success for the same destination in one pass.
- A later successful delivery resolves the destination incident. Recovery
  notifications use only destinations that received the failure notification.

The policy returns incident changes, whether a failure became notifiable, and
whether recovery notifications were queued. `watch` uses these facts to keep its
existing event names and fields. If an operation fails after some changes were
saved, the returned changes let `watch` report that partial result.

This change does not alter the scheduler, Telegram retry rules, database schema,
or the command JSON schema.
