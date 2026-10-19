# Card actions release

The production user frontend is `https://redeem.baochai.cc/sharegpt/`, served by
the `baochai-redeem` Cloudflare Pages project and its Pages Worker. The existing
`REDEEM_PROXY` service for the other recharge systems must remain unchanged.
`web/public/user.html` and its assets are the canonical Pages frontend sources.
The server hosts the allocation API and admin UI, not this new frontend release.

## Rules

- Different-account redemption links an activated original code and an unused
  new code into a persistent group. New and future replacement allocations avoid
  normalized logins currently occupied by other group members' primary or
  unexpired grace allocations. Historical ended accounts may be reused.
- Renewal transfers all duration from an unused new code to the original. An
  unused original stays unused; an active original extends its previous expiry;
  an expired original restarts now, preferring a usable previous account. If
  unavailable, the system can allocate another account subject to group rules.
- Successful renewal marks the source `consumed`. It cannot be redeemed,
  extended or revoked as an independent code. Generation and a single admin
  extension remain 1–90 days; accumulated duration has no 90-day business cap.
- Preview rolls back its entire transaction. Confirmation rechecks availability.
  A unique operation per source code makes same action/target retries idempotent.
  Different targets/actions cannot reuse the source. Failures roll back capacity,
  grouping, duration, source status, and the operation/audit ledger together.
- Public responses never expose card/group IDs or member/account credentials.
  Credential lookup remains the existing separately protected query API.

## Verify and build

1. Run allocation-service and root Go tests, `web` lint/tests/build, and Pages
   worker/branding tests. `card-actions-smoke.mjs` uses intercepted synthetic API
   responses for previews, CAPTCHA, renewals, distinct accounts and mobile UI.
2. After `npm run build`, run `preserve-origin-ui.mjs` to retain the currently
   served server public page. This explicitly prevents publishing the new user
   frontend on the origin. Publish `web/public` through `build.mjs` instead.
3. Build `deploy/Dockerfile.card-actions` or compile the same two binaries with
   cached Go modules and copy both into the current production runtime image.
   The application AND migrator must contain schema 18.
4. Back up both server databases, configuration, running image identity and
   original public assets, with private permissions and no credential output.
5. Deploy the Pages preview branch first and run both existing-provider smoke
   tests and `card-actions-smoke.mjs https://preview-host/sharegpt/`. These tests
   intercept transactions and do not consume real codes.
6. Switch only the server application container, preserving production env,
   mounts and resource limits. Verify schema 18, SQLite integrity/FKs, health,
   unchanged original public page and new API method handling. Then publish the
   identical Pages artifact on `main` and repeat browser checks.

The origin nginx uses server-level Cloudflare Access protection. Keep that
protection and add only the three exact new public API locations with
`auth_request off`, mirroring the existing query/redeem locations. Do not disable
Access for `/api/` or `/admin/`. Save the current nginx config, run `nginx -t`,
reload gracefully, verify both the new proxy chain and unauthenticated admin
denial. The repository nginx template preserves the existing Access include.

## Safe rollback

Save the previous Pages production deployment ID and roll back only this Pages
project. Leave the independent recharge Worker and its binding intact.

After schema 18 or paid transactions exist, **never restore the old database or
start a pre-schema-18 binary**. The rollback backend stays schema-compatible:
set `PUBLIC_CARD_ACTIONS_ENABLED=false` and restart only the application container.
The three new endpoints return 503; ordinary queries and admin remain available.
Remove the flag or set it true to re-enable. Keep transaction data and ledger.
