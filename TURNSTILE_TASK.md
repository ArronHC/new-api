# FlareSolverr Turnstile Token Extraction for Auto-Checkin

## Problem
The current auto-checkin code in `model/auto_checkin.go` has a FlareSolverr integration that detects Cloudflare challenge pages (403/503) and retries with cf_clearance cookies. However, the actual issue is different:

1. The upstream New API checkin endpoint `GET /api/user/checkin` returns valid JSON (status 200) — no Cloudflare block
2. The `POST /api/user/checkin` requires a `turnstile_token` field in the JSON body
3. Current code sends POST with `nil` body → upstream returns "Turnstile token 为空"

## What Needs to Change

### FlareSolverr Integration Approach
Instead of using FlareSolverr as an HTTP proxy to get cookies, we need to:

1. Use FlareSolverr to **navigate to the upstream site's main page** (e.g., `https://v-api.de5.net`)
2. FlareSolverr solves the Turnstile widget embedded on the page (it presses Tab to trigger verification)
3. **Extract the `turnstile_token`** from the FlareSolverr response — look for `turnstile_token` field in the solution, or parse `cf-turnstile-response` from the HTML response
4. Send that token in the POST body: `{"turnstile_token": "<token>"}`

### FlareSolverr API
```json
POST http://flaresolverr:8191/v1
{
  "cmd": "request.get",
  "url": "https://target-site.com",
  "maxTimeout": 60000
}
```

Response includes:
- `solution.turnstile_token` — the solved turnstile token (may be null if no turnstile)
- `solution.response` — the HTML content of the page
- `solution.cookies` — cookies including cf_clearance

### Code Changes Needed

1. **Replace `isCloudflareBlock` detection** — the upstream returns valid JSON, not a Cloudflare page. Instead, detect the "Turnstile token 为空" error message in the POST response.

2. **New function `getTurnstileToken(ctx, baseURL)`**:
   - Calls FlareSolverr with the upstream site's base URL
   - Parses the response for `turnstile_token` field
   - Also tries to extract from HTML response if `turnstile_token` is null
   - Caches the token per domain (token has a short TTL, ~2-3 minutes)

3. **Modify POST checkin request**:
   - When POST returns "Turnstile token 为空", call `getTurnstileToken()`
   - Retry POST with JSON body: `{"turnstile_token": "<token>"}`
   - Set `Content-Type: application/json` on POST

4. **Clean up unused Cloudflare detection** — the `isCloudflareBlock` function and `executeCheckinHTTPWithFlareSolverr` should be refactored since the issue isn't a Cloudflare challenge page.

### Important Notes
- The current code already works for sites that don't require Turnstile (channels 3, 4, 7, 10 already checked in successfully)
- The `isCloudflareBlock` function is still useful as a fallback for sites that actually have Cloudflare challenge pages
- FlareSolverr is already deployed and accessible at `http://flaresolverr:8191` inside Docker
- The `FLARESOLVERR_URL` env var is set in the container
- The Go code is in `/home/arronhc/projects/new-api/model/auto_checkin.go`
- The project AGENTS.md at `/home/arronhc/projects/new-api/AGENTS.md` has coding conventions — MUST read it first
- Use `common.Marshal`/`common.Unmarshal` for JSON operations (not `encoding/json`)
- All database code must support SQLite, MySQL, PostgreSQL
- The project uses Go 1.22+, Gin, GORM v2

## Files to Modify
- `model/auto_checkin.go` — main checkin logic
- Possibly `model/auto_checkin_test.go` — add test for Turnstile flow

## Test
After changes, trigger manual checkin:
```bash
curl -s -X POST 'http://127.0.0.1:3001/api/user/auto-checkin/trigger' \
  -H 'Authorization: Bearer <access_token>' \
  -H 'New-Api-User: 1'
```
Channels 8 (V-api) and 11 (HLOOL_gpt) should succeed if Turnstile is solved.
