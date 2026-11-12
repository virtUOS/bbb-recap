# Identity mapping

Who receives the summary, and how the service names that person to La Suite Docs. Decisions:
[ADR-006](../decisions/ADR-006-identity-owner-only.md) (owner only, through the registration),
[ADR-007](../decisions/ADR-007-docs-sink-bot-user.md) (Docs `create-for-owner`),
[ADR-009](../decisions/ADR-009-privacy-retention.md). Registration wire format:
[`metadata-interface.md`](metadata-interface.md) §2. Docs call: [`sinks.md`](sinks.md) §3. Docs sources are
`suitenumerique/docs` at `6c730f7` (paths below `src/backend/`); see also [`upstream-facts.md`](upstream-facts.md).

## 1. Principle

- BBB carries no portable identity. Each front end sends its own `userID` (PILOS `u<int>`, Moodle numeric id,
  Greenlight `gl-<uuid>`, Stud.IP none → BBB `w_<random>`). None sends an email or OIDC `sub` (read in `THM-Health/PILOS` @
  `b2a3128`, `moodle/moodle` @ `46c042e`, `bigbluebutton/greenlight` @ `bf963d2`, `elan-ev/studip-meeting` @ `2bcabad`;
  [`upstream-facts.md`](upstream-facts.md)).
- **One recipient, the owner**, named by the front end in the registration.
- The service never derives identity from meta, display names or `userID`s, and never sends identity into BBB.

## 2. Owner identity

- **`owner.sub`**: the subject Docs stores for the user. Docs matches `create-for-owner` by `sub` first, then by email
  (`core/models.py:118-133 @ 6c730f7`). Docs requires both fields, and `sub` has at most 255 characters
  (`core/api/serializers.py:453-465 @ 6c730f7`).
- **`owner.email`**: as the front end stores it. Docs matches it case-insensitively when the `sub` does not match and
  `OIDC_FALLBACK_TO_EMAIL_FOR_IDENTIFICATION=true` (the default, `impress/settings.py:795-799 @ 6c730f7`). For a
  matched user Docs uses its own stored email (`core/api/serializers.py:488-490 @ 6c730f7`).
- The front end and Docs should use the **same OIDC provider**, so the front end can send the `sub` that Docs stores.
- If the `sub` does not match but the email does, delivery still reaches the right account through the email
  fallback. If neither matches, Docs invites the email ([`sinks.md`](sinks.md) §3.3).
- **Who is the owner**: the room owner in the front end. Only the room owner may opt in.
- The front end may correct the owner with a new `PUT` until delivery starts
  ([`metadata-interface.md`](metadata-interface.md) §2.4).

## 3. Data items and privacy

| Item                                           | From → to                  | Stored                                          | Retention (default)                                                                                                                               | Deleted by                                     | Logged   |
| ---------------------------------------------- | -------------------------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- | -------- |
| `owner.sub`                                    | front end → service → Docs | service Postgres (`registrations`); Docs `User` | service: deleted at `cleanup` after delivery; unmatched until `RECAP_REGISTRATION_TTL` (14d); held/failed until an admin acts. Docs: Docs' policy | service `cleanup`, cleanup loop, admin `purge` | never    |
| `owner.email`                                  | same                       | same; Docs `Invitation` for unknown owners      | same; Docs invitation rows: see Known limitations                                                                                                 | same; Docs operator                            | never    |
| `meeting_id`, `record_id`, `frontend_id`       | front end, BBB             | job metadata                                    | `RECAP_JOB_METADATA_RETENTION` (180d)                                                                                                             | cleanup loop                                   | IDs only |
| Participants' `externalUserId`, `name`, `role` | `events.xml`               | package blob, transcript                        | job content, deleted at `cleanup`                                                                                                                 | service `cleanup`                              | never    |

Participant data is used only for speaker labels ([`providers.md`](providers.md)), not for delivery. Front ends must
not put any of these into `meta_*` ([`metadata-interface.md`](metadata-interface.md) §1.2).

## 4. Outcomes at delivery

| Docs result (`core/models.py:118-143`, `core/api/serializers.py:485-522 @ 6c730f7`) | Job                                                 |
| ----------------------------------------------------------------------------------- | --------------------------------------------------- |
| `sub` or email matched one user → owner access                                      | `delivered`                                         |
| No match → owner invitation and Docs email                                          | `delivered` (the service cannot tell the two apart) |
| Two active users share the email → 400                                              | held `owner_unresolved`                             |

## 5. Who may change the owner

Only the registering front end, via `PUT` before delivery ([`metadata-interface.md`](metadata-interface.md) §2.4). A
held `owner_unresolved` job resumes after that correction. An admin can also `release` the job once the Docs side is
fixed (for example a duplicate account removed), or `purge` it ([`README.md`](README.md) admin actions). There is no
admin owner edit.

## 6. PILOS notes (`THM-Health/PILOS` @ `b2a3128`)

- `userID` on join is `'u' . Auth::user()->id` (`app/Services/MeetingService.php:677`). Guests get
  `'s' . session()->getId()`, a session secret that BBB writes into `events.xml`.
- PILOS supports LDAP, Shibboleth and OIDC. `external_id` and `email` are required user attributes
  (`app/Auth/ExternalUser.php:104-117`). The documented OIDC example maps `external_id` from `preferred_username`, not
  `sub` (`docs/docs/administration/08-advanced/01-external-authentication.md:219-264`).

| Registration field | PILOS source                                                                                                                                                                               |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `meeting_id`       | `meetingID` sent on `create`                                                                                                                                                               |
| `record_id`        | `internalMeetingID` of the `create` response, read through the PHP BBB library (`littleredbutton/bigbluebutton-api-php` v6.2.2, `src/Responses/CreateMeetingResponse.php:38-41 @ 0178d70`) |
| `owner.sub`        | OIDC `sub` claim of the room owner, saved by the PILOS integration at login in a new field (`external_id` stays unchanged)                                                                 |
| `owner.email`      | `users.email` of the room owner                                                                                                                                                            |

- PILOS users from LDAP, Shibboleth or local accounts have no OIDC `sub` in PILOS, so they cannot opt in.
- Other front ends: Greenlight stores the OIDC `sub` as `external_id` by default (`bigbluebutton/greenlight` @
  `bf963d2`).

## 7. Guests

Guests can never be owners: a registration names an authenticated front-end user. Guest IDs in `events.xml` (PILOS
`s<session>`, Greenlight `gl-guest-<hex>`, BBB `w_<random>`; BBB: `ApiController.groovy:469,479-481 @ c845dac`) only
group audio tracks into speakers. They are never logged, matched or sent to a sink.

## 8. Later: participant map

Not supported yet. A possible extension ([ADR-006](../decisions/ADR-006-identity-owner-only.md) option d):

- Optional `participants` list in a later registration version: `external_user_id`, `sub`, `email`, `role`.
- Matched against `ParticipantJoinEvent@externalUserId` in `events.xml`. Only people who joined get access; guests
  are excluded. Unmatched entries are deleted.
- Needs Docs access management beyond `create-for-owner`, participant notice and consent, and the operator's legal
  assessment ([ADR-009](../decisions/ADR-009-privacy-retention.md)). Same retention as the owner identity.

## 9. Contract tests

| ID       | Pins                                                                                            |
| -------- | ----------------------------------------------------------------------------------------------- |
| CT-ID-01 | The Docs request carries exactly the registration's `sub` and `email`, unchanged.               |
| CT-ID-02 | Mock Docs duplicate-email 400 → held `owner_unresolved`; a registration `PUT` resumes the job.  |
| CT-ID-03 | After `cleanup`, no `sub` or email of the job remains in Postgres; job metadata keeps only IDs. |
| CT-ID-04 | Logs of CT-ID-01…03 contain no fixture email, `sub` or `externalUserId`.                        |

## 10. Known limitations

- PILOS's `external_id` is often `preferred_username`, not `sub`. The PILOS integration therefore stores the OIDC
  `sub` separately; Docs' email fallback covers a mismatch. Which claim a given Docs instance stores as `sub` depends
  on its OIDC setup.
- Users without an OIDC `sub` in the front end (LDAP, Shibboleth, local accounts) cannot opt in; a made-up `sub`
  could match the wrong Docs user.
- An owner invitation in Docs is valid for 7 days (`INVITATION_VALIDITY_DURATION`, `impress/settings.py:548 @
6c730f7`). If the owner does not log in within that time, the document has no owner, and the service cannot fix
  this. Whether Docs ever purges invitation rows was not checked.
- How Moodle and Stud.IP single sign-on IDs map to an OIDC `sub` was not checked.
