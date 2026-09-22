# multica-github-dispatcher

`multica-github-dispatcher` connects GitHub pull requests to an AntMultica
workspace without an inbound webhook.

It runs two independent polling lanes:

1. A new open PR, or a new head SHA on an existing PR, creates one review
   round. The PR title must start with `[WANG-N]`. Reviewers are selected
   deterministically from the configured Multica squad and mentioned on the
   existing Multica issue.
2. A GitHub inline review comment whose first non-empty line starts with
   `multica:fix`, or a `CHANGES_REQUESTED` review, mentions the issue's current
   assignee. This lane does not depend on the PR having been dispatched for
   review by this service.

The service never exposes an HTTP port. GitHub is read through its REST API;
Multica writes are performed by the `multica` CLI.

## Build and test

Go 1.25 or newer is required.

```bash
go test ./...
go vet ./...
go build -o multica-github-dispatcher ./cmd/multica-github-dispatcher
```

## Configuration

Copy [`deploy/config.example.yaml`](deploy/config.example.yaml) and set the
workspace, repository, and reviewer squad IDs. Secrets are references, not
literal YAML values.

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

The service validates `GET /rate_limit` through the selected route and validates
the Multica workspace before migrating SQLite or polling. GitHub failures do
not advance cursors.

## Credentials

GitHub uses the configured `GITHUB_TOKEN` to authenticate REST requests and
avoid the unauthenticated rate limit. Read-only repository metadata and pull
request permissions are sufficient for the dispatcher. Reviewer agents use
their own existing `gh` authentication to publish reviews.

Multica supports either an environment variable or a token file. A token file
must be a regular file with mode `0600` or stricter and is read before every CLI
call, so an atomic replacement rotates it without restarting the service.

Create a private environment file from
[`deploy/dispatcher.env.example`](deploy/dispatcher.env.example), then set mode
`0600`. Never commit that file.

## Commands

```bash
multica-github-dispatcher once --config /path/to/config.yaml --dry-run
multica-github-dispatcher once --config /path/to/config.yaml
multica-github-dispatcher run --config /path/to/config.yaml
multica-github-dispatcher status --config /path/to/config.yaml
```

`--dry-run` performs live GitHub and Multica startup checks and discovery, but
uses an in-memory state database and sends no Multica comments.

## State and idempotency

SQLite runs in WAL mode. A review round is keyed by
`owner/repo + PR number + head SHA`. GitHub feedback is keyed by `comment.id`
or `review.id`, so editing a previously processed comment does not re-trigger
work. A five-minute overlap on the per-repository review-comment cursor avoids
boundary loss. Multica comments carry HTML markers; the outbox checks those
markers before retries to prevent duplicate mentions after an ambiguous write.

## OCR reviewer runtime

The reviewer runtime needs OCR 1.12.8 and the Codex plugin:

```bash
npm install -g @alibaba-group/open-code-review@1.12.8
codex plugin marketplace add alibaba/open-code-review --ref v1.12.8
codex plugin add open-code-review-codex@open-code-review --json
```

Only agents that have passed a local `ocr --version`, delegate preview, and
Codex plugin readiness check should be added to the configured review squad.
The squad is therefore the dispatcher-side readiness allow-list. Reviewer and
worker instruction templates are in [`deploy/agent-instructions`](deploy/agent-instructions).

## Deployment

[`deploy/multica-github-dispatcher.service`](deploy/multica-github-dispatcher.service)
is a user-service template for the dev host. It has no inbound socket, restarts
on failure, logs JSON to journald, and uses a process lock next to the database.

```bash
systemctl --user daemon-reload
systemctl --user enable --now multica-github-dispatcher.service
journalctl --user -u multica-github-dispatcher.service -f
```
