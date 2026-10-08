# multica-github-dispatcher

`multica-github-dispatcher` discovers GitHub pull requests independently of
Multica issues and starts reviewer Agents through `run_only` Autopilot
webhooks. A Multica issue is optional context, not a prerequisite for review.

It runs two independent polling lanes:

1. A new open PR, or a new head SHA on an existing PR, creates one review
   round. Reviewers are selected deterministically from the repository's
   configured reviewer IDs and invoked through their webhook. If the PR title
   or body contains a key such as `WANG-2`, the dispatcher fetches that issue
   and adds its context to the review payload. A clean review creates no GitHub
   or Multica comment.
2. An actionable inline finding carries a hidden
   `automated-review-finding:v1` HTML marker while keeping ordinary visible
   review text. If the PR has an associated active Multica issue, that finding
   or a `CHANGES_REQUESTED` review mentions the issue's current assignee with a
   link to GitHub; the GitHub review body is not copied into the issue. Mention
   routing uses the assignee ID, so a private Agent can still be awakened even
   when the dispatcher's member-scoped token cannot list its display name.

The dispatcher never exposes an inbound event port. GitHub is read through its
REST API, reviewer webhooks are outbound HTTP(S) requests, and optional Multica
context/feedback operations use the `multica` CLI. A read-only dashboard is a
separate process with a configurable listener.

## Build and test

Go 1.25 or newer is required.

```bash
go test ./...
go vet ./...
go build -o multica-github-dispatcher ./cmd/multica-github-dispatcher
```

## Configuration

Copy [`deploy/config.example.yaml`](deploy/config.example.yaml), configure the
repositories and reviewer IDs, and map every reviewer to a secret-backed
Autopilot webhook URL. Secrets are references, not literal YAML values.

The GitHub proxy is entirely user supplied. The dispatcher does not create or
modify a proxy:

- `type: none` always connects directly.
- `type: standard_env` uses `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY`.
- `type: url_env` reads one dedicated variable such as
  `GITHUB_PROXY_URL`. It accepts `http://`, `https://`, `socks5://`, and
  `socks5h://` URLs, including authenticated URLs.

With `required: true`, an empty proxy variable is a startup error and never
falls back to direct access. Logs expose only the proxy scheme, host, and port.
The GitHub client has its own transport. Every Multica CLI child explicitly
removes standard proxy variables, so the GitHub route cannot accidentally be
used for `antmultica.alipay.com`.

The service validates `GET /rate_limit` through the selected route and resolves
all reviewer webhook secrets before migrating SQLite or polling. Multica
workspace validation is optional: a failure removes issue context and feedback
routing for that cycle but does not stop independent PR review. GitHub failures
do not advance cursors.

## Credentials

GitHub uses the configured `GITHUB_TOKEN` to authenticate REST requests and
avoid the unauthenticated rate limit. Read-only repository metadata and pull
request permissions are sufficient for the dispatcher. Reviewer agents use
their own existing `gh` authentication to publish reviews.

Multica supports either an environment variable or a token file. A token file
must be a regular file with mode `0600` or stricter and is read before every CLI
call, so an atomic replacement rotates it without restarting the service.

Each `run_only` Autopilot webhook URL is an opaque credential. Store it through
an environment or mode-`0600` file reference. The URL is resolved on every
delivery, never written to SQLite, and redacted from network errors. Webhook
requests bypass GitHub proxy settings.

Create a private environment file from
[`deploy/dispatcher.env.example`](deploy/dispatcher.env.example), then set mode
`0600`. Never commit that file.

## Commands

```bash
multica-github-dispatcher once --config /path/to/config.yaml --dry-run
multica-github-dispatcher once --config /path/to/config.yaml
multica-github-dispatcher run --config /path/to/config.yaml
multica-github-dispatcher dashboard --config /path/to/config.yaml
multica-github-dispatcher status --config /path/to/config.yaml
```

`--dry-run` performs live GitHub and reviewer configuration checks and
discovery, but uses an in-memory state database and sends no reviewer webhook
or Multica comment.

## State and idempotency

SQLite runs in WAL mode. A review round is keyed by
`owner/repo + PR number + head SHA`. GitHub feedback is keyed by `comment.id`
or `review.id`, so editing a previously processed comment does not re-trigger
work. A five-minute overlap on the per-repository review-comment cursor avoids
boundary loss. Review webhook requests carry the round event key in both JSON
and the `Idempotency-Key` header. Multica feedback comments carry HTML markers;
the outbox checks those markers before retries to prevent duplicate mentions
after an ambiguous write.

## Read-only dashboard

The dashboard shows repository and PR review rounds, linked Multica issues,
GitHub feedback, delivery retries, cursors, recent poll health, and webhook
configuration status for each selectable reviewer. It reads SQLite directly
and does not require GitHub or Multica credentials.
Set `multica.workspace_url` to the public workspace URL so issue identifiers in
the review-round table link to their Multica issue pages. Reviewer display names
come from `review_dispatch.agents`; UUIDs remain available only as hover text
for diagnostics. PRs without an associated issue show `未关联`.

The listener defaults to `127.0.0.1:8787`. Set it to `0.0.0.0:8787` to expose
the dashboard on every dev-host IPv4 interface, then open
`http://<dev-host>:8787` directly. The dashboard has no built-in authentication,
so a wildcard listener should only be used on a trusted network or with an
external firewall.

For a loopback listener, access it from a workstation with:

```bash
ssh -N -L 8787:127.0.0.1:8787 dev
```

Then open `http://127.0.0.1:8787`. The UI auto-refreshes and exposes only a
read-only JSON endpoint. External comment bodies are truncated and rendered as
text, and security headers prevent third-party scripts and framing.

## OCR reviewer runtime

The reviewer runtime needs OCR 1.12.8 and the Codex plugin:

```bash
npm install -g @alibaba-group/open-code-review@1.12.8
codex plugin marketplace add alibaba/open-code-review --ref v1.12.8
codex plugin add open-code-review-codex@open-code-review --json
```

Only agents that have passed a local `ocr --version`, delegate preview, and
Codex plugin readiness check should be added to `review_dispatch.agents` and to
a repository's `reviewer_ids`. Autopilot owns runtime execution; the dispatcher
does not create or comment on a Multica issue to start review. Reviewer and
worker instruction templates are in
[`deploy/agent-instructions`](deploy/agent-instructions).

## Deployment

[`deploy/multica-github-dispatcher.service`](deploy/multica-github-dispatcher.service)
and [`deploy/multica-github-dashboard.service`](deploy/multica-github-dashboard.service)
are user-service templates for the dev host. Both restart on failure and log to
journald. The dispatcher uses a process lock next to the database; the dashboard
is read-only and listens on the address configured by `dashboard.listen`.

```bash
systemctl --user daemon-reload
systemctl --user enable --now multica-github-dispatcher.service
systemctl --user enable --now multica-github-dashboard.service
journalctl --user -u multica-github-dispatcher.service -f
```
