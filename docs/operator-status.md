# Operator status

`internal/operatorstatus` reads the state used by `history status`, `doctor`,
and the Polish terminal view. It reads SQLite and the selected session store.
It does not prompt, send requests, or change saved data. Callers control history
pruning before the read.

The shared module owns these rules:

- A missing, corrupt, expired, or unrelated session requires login.
- An active account authentication incident requires login even when saved
  cookies are usable. The account has one `authentication_required` action.
- An unsafe or unavailable session store leaves authentication unknown and
  adds `session_unavailable`. An active authentication incident takes priority.
- Disabled profiles and destinations require action. A failed destination test
  adds `destination_test_failure`.
- Permanent delivery failures require action. Each affected destination has
  one `destination_delivery_failure` action, including when both availability
  and incident deliveries fail.
- A delivery cancelled because a slot disappeared or an incident ended stays
  in history but does not require action.

The command adapter owns JSON fields, text, summary counts, and command filters.
The terminal adapter owns Polish labels and navigation. The JSON schema version,
field names, and historical delivery statuses stay the same. Historical failure
lists and their counts still include cancellations stored as `permanent_failure`.
The shared read uses the existing limit of 1,000 rows for each incident or failure
query. The new action codes above also appear in command output.
