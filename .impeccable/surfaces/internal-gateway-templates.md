---
version: 1
slug: "internal-gateway-templates"
primary_target: "internal/gateway/templates"
related_targets: ["internal/gateway/static/portal.css"]
---

# Surface: the gateway portal (catalog, sign-in, account, messages)

Mode: Operate. Primary user: the operator, on a phone, reaching their own apps; other accounts are occasional.
Job: one tap from the portal into a running app; anything down is obvious, with its reason and the command to inspect it.
Proof/content: only manifest fields and what the daemon observes (status, halted, restart count). No invented metrics.
Constraints: embedded in the binary, nothing fetched; CSP default-src 'self'; WCAG 2.2 AA; phone first; night only.
Scope: design PR 3 (docs/design/gateway-users-catalog.md) plus the 2026-10-29 critique fixes.
Open: icon serving deferred; light rendition not built (night is the world).

## Direction contract

THESIS: The Outpost header made into the working page: service health drawn as one transit line running from the lamp to a station per app, where an app that is down is a broken stretch of line. Refuses the homelab icon-tile grid and the bare link list.

OWN-WORLD: Night-enamel ground (#141A21 family), porcelain ink (#F3F1E9), beam-teal line (#79A59D, lightened from the header for 6.4:1 text contrast), lamp amber (#F1C191) only for the hub and focus, warm red (#D9583B) only on down stations. Porcelain station rings, a 6px rounded line, system humanist sans at fixed label sizes, monospace for addresses and commands. No cards, no shadows beyond the lamp glow.

STORY: The operator sees at once how many apps are up, taps a running station to land in it, and on a down one reads why and the exact `outpost status` command; account and sign-out sit in one rail and never compete with the line.

FIRST VIEWPORT: Top rail: host (mono, left) and the account control (right, 44px). Hub: the lamp disc beside the portal name (~28px) with "N running · M down". Below, the line down the left edge, one 72px station row per app: name 21px, description and right-aligned mono address beneath; running first, then starting, then down (broken line, reason, command); hidden apps tagged for admins.

FORM: Service Line, fused from the midnight transit-diagram challenger with the brand header; it beat my rolled candidate 7 (Bookshelf of Spines) on both axes. Seed key 0bc23494 (re-roll 1).

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
