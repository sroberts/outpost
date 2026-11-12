# Mail for apps

Apps get and send email through one Gmail account, each at its own plus
address. The app's side is `outpost-service-spec.md` §16; the operator's is
the README's Mail section. This is why it's built the way it is.

## Routing by plus address

Gmail delivers `you+anything@gmail.com` to `you@gmail.com`, and ignores dots
in the local part and the difference between gmail.com and googlemail.com.
So the account needs no setup per app, and an address can be made or
retired without telling Google anything. A route is either:

- **an app's name,** when the name is safe in an address (lower-case
  letters, digits, `-`, `_`, `.`). It needs nothing stored.
- **an alias,** four characters from an alphabet without look-alikes, with at
  least one digit, so it can't collide with an app's name made of words. It
  lives in `mail.db` with a note. An alias is checked first.

The recipient is read from `Delivered-To` first, which Gmail sets to the
address that actually received the message, including a Bcc, then
`X-Original-To`, `To` and `Cc`.

## Reading the inbox

IMAP over TLS with an app password: pure Go (`emersion/go-imap` v1), no
Google Cloud project, no OAuth consent screen, and no refresh token that
expires after seven days in an unpublished app. The Gmail API would need all
of those.

Each check:

1. selects `INBOX` and reads its `UIDVALIDITY`;
2. searches for UIDs above the last one looked at, or, for a new account or
   a new `UIDVALIDITY`, everything received in the last seven days;
3. fetches only the recipient headers of those, with `BODY.PEEK`, so nothing
   is marked read;
4. fetches whole bodies only of messages with a route;
5. keeps each in `mail.db`, keyed by `uidvalidity:uid` so a message is never
   kept twice;
6. moves the cursor, then labels each `Outpost/<app>` and takes `\Inbox`
   away with `X-GM-LABELS`, which archives it.

The cursor moves before labelling, so a label that fails leaves mail in the
inbox rather than routing it twice. A batch is at most 200 messages; the
next follows at once. Polling every minute is simpler than IDLE and quick
enough for newsletters and notifications.

## The endpoint

The same shape as the AI endpoint: a loopback HTTP server in the daemon,
`OUTPOST_MAIL_URL`, and a per-app `OUTPOST_MAIL_TOKEN` that is an HMAC of
the app's name and a generation, so it's never stored and survives restarts.
Replacing it bumps the generation, which changes the app's environment
revision, so the supervisor restarts it with the new one.

Feeds can't carry a token, since a feed reader sends none, so each feed link
carries an HMAC of its route under a second key. It lasts as long as the
route. The message view each entry links to is served with
`Content-Security-Policy: sandbox`, so nothing in a newsletter runs.

## Sending

SMTP to `smtp.gmail.com:587` with STARTTLS, refusing a server that doesn't
offer it. The envelope sender is the account, as Gmail requires. The `From`
and `Reply-To` headers are the app's plus address, so a reply routes back to
the app. Sending is off for each app until an admin allows it, and limited
to 100 messages a day per app, because Gmail stops the whole account sending
for a day after about 500.

## What isn't here yet

- Attachments, in either direction.
- IMAP IDLE, for mail within seconds rather than a minute.
- An `outpost mail` CLI; for now the Mail page, or `[mail]` in config.toml
  and `outpost secret set gmail-app-password`.
- Push to apps by webhook. Apps poll, and feed readers subscribe.
