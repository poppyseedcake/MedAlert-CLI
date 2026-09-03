# Appointment Monitoring

This context describes how MedAlert observes available Medicover time slots and informs a user. It does not book or manage visits.

## Language

**Medicover Account**:
A user identity that can access Medicover Online. Each account owns its authentication state and observation profiles.
_Avoid_: User, Login

**Authentication Required**:
The state of one Medicover account when MedAlert cannot renew its session without user action. Observation profiles for that account pause without changing availability episodes, while other accounts continue.
_Avoid_: Logged out, Account failure

**Secret Source**:
The protected location from which MedAlert reads a Medicover account password or a notification route token. It is a system Secret Service entry, a mounted secret file, or a temporary hidden prompt, but never SQLite, a plain configuration file, or a direct environment-variable value.
_Avoid_: Secret configuration, Credential file

**Medicover Session State**:
The protected, mutable cookies, refresh token, and trusted-device data for one Medicover account. It persists in Secret Service or in one restricted Docker session file and excludes the short-lived access token.
_Avoid_: Login cache, Cookie file

**Observation Profile**:
A stable observation identity with saved appointment search criteria and notification routes for one Medicover account. Disabling it pauses observation without ending its history; changing criteria keeps active episodes for slots that still match.
_Avoid_: Watch, Search configuration, Job

**Observation Run**:
One complete evaluation of one saved version of an observation profile against the available slots returned by Medicover. One profile has at most one active run; a stale, partial, or failed evaluation does not change availability episodes or create notifications.
_Avoid_: Scan, Poll

**Observation Failure**:
The active failure state that starts with the first failed observation run and ends with the next complete, successful run. Repeated failures update one continuous failure record and do not change availability episodes.
_Avoid_: Empty result, No appointments

**Observation History**:
The retained record of completed observation runs, observation failures, availability episodes, and delivery attempts for one observation profile. Deleting the profile deletes its observation history.
_Avoid_: Logs, Cache

**Available Slot**:
A unique time that Medicover currently offers for a visit. Equivalent records with the same slot identity describe one available slot; conflicting records make the observation run invalid.
_Avoid_: Appointment, Visit

**Slot Identity**:
The stable identity of an available slot across observation runs. It uses `bookingString` when present and otherwise uses the slot time, clinic, doctor, specialty, and visit type; a change between these representations does not create a new identity.
_Avoid_: Appointment ID, Database ID

**Availability Episode**:
One continuous period during which an available slot remains visible to one observation profile. It continues across restarts until a complete successful run no longer finds the slot, the slot time passes, or the profile is deleted; each matching profile has independent episodes and delivery state.
_Avoid_: Hit, Event

**Notification Route**:
A stable configured destination through which the user can receive availability notifications. A destination or provider change creates a new route that is eligible for each active episode; presentation-only changes keep the route identity.
_Avoid_: Notifier, Channel

**Delivery Attempt**:
One recorded provider call that tries to send one availability, operational, or test notification through one notification route. An unknown result can be repeated after recovery, so delivery is at least once; pending availability attempts stop when their availability episode ends.
_Avoid_: Message, Alert

**Operational Incident**:
One continuous account, observation profile, or notification route failure that can produce one failure notification and one recovery notification per eligible route. Repeated failures update the same incident and do not create more notifications.
_Avoid_: Error alert, Failure message

**Compatibility Contract**:
The user-visible search and notification behavior that MedAlert preserves from MediCzuwacz. It excludes compatibility with command names, configuration files, and persisted data.
_Avoid_: File compatibility, CLI compatibility
