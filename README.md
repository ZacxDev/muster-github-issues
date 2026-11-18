# muster-github-issues

A [muster](https://github.com/ZacxDev/muster) plugin that turns **GitHub
issues into muster tasks**.

It polls the repositories you name, with a GitHub token that can only read
issues. Each new issue (or each new issue carrying one of your labels) becomes
one muster task with the issue's title, body, link and labels, handed to your
**chief** as "assigned". Later changes to the issue (a comment, a label, a
close) are added to that same task as comments. Nothing is filed twice, even
across restarts. It needs no public URL: it calls GitHub and muster, and
nothing calls it.

**Tested against** muster commit `d8d3dca3e` (main, 2026-11-18): the run in
"Run it in 5 minutes" below, against a throwaway muster with
`MUSTER_PLUGINS=1` and `MUSTER_TASK_ROUTING=1` and a stand-in GitHub on
loopback. It filed the task (routed to the chief), filed nothing on repeated
polls or after a restart, and delivered an update queued while muster was down
once muster came back.

## Run it in 5 minutes

You need Go (the version in `go.mod`) and a muster you can sign in to.

1. **On the muster server**, turn plugins on and name the task namespace this
   plugin files under. In the `.env` beside muster's `docker-compose.yml` add

   ```sh
   MUSTER_PLUGINS=1
   MUSTER_PLUGIN_TASK_NAMESPACES=github   # add to any list you already have
   MUSTER_TASK_ROUTING=1                  # hands each new issue to your chief
   ```

   and restart muster (`docker compose up -d --wait`).
2. **Create the plugin** on muster's `/plugins` page (name `github-issues`,
   label `GitHub issues`) and copy the `mpl_…` token it shows once.
3. **Create a GitHub token**: Settings → Developer settings → Fine-grained
   tokens → Generate. Repository access: only the repositories you want.
   Permissions: **Issues: Read-only** (GitHub adds Metadata: Read-only). Nothing
   else.
4. **Configure and start:**

   ```sh
   cp .env.example .env
   # edit .env: MUSTER_URL, MUSTER_PLUGIN_TOKEN, GITHUB_TOKEN, GITHUB_REPOS (owner/name, ...)
   set -a; . ./.env; set +a
   go run .
   ```

   It logs the plugin's name, then `polling <repos> every 1m0s`. Open an issue
   in one of the repositories: within a poll it logs
   `event github:<owner>/<repo>/<n>@<time>: task <id> (created true, commented true, routed chief)`
   and the task is on your muster board, tagged `github:<owner>/<repo>/<n>`.

By default it files only issues updated after its first start. Set
`GITHUB_SINCE` before the first start to file older ones; that backfill
reaches your chief at `GITHUB_MAX_PER_HOUR` (60 an hour after a burst of 10
by default), not all at once.

At startup it checks with muster (`GET /plugin/v1/tasks?namespace=github&live=true`)
that the namespace is in the server's `MUSTER_PLUGIN_TASK_NAMESPACES`. If it
is not, it refuses to start and says which variable to change, rather than
having every task refused one by one.

## Security: untrusted text

Issue titles, bodies and comments are written by anyone who can open or
comment on an issue in the repositories you poll. To this plugin they are
**outside data, never instructions**.

muster, not the plugin, fences plugin-filed task text, so the rule lives in
one place. That is muster PR
[#824](https://github.com/ZacxDev/muster/pull/824), which is **not merged** at
the time of writing; no muster release fences this text yet. Until a release
carrying #824 is out, an agent started on one of these tasks reads the
issue text unfenced.

Released muster can start agents on these tasks without you. With
`MUSTER_TASK_ROUTING=1` (the quickstart above turns it on) each new task is
handed to your chief, and the chief's `agent_dispatch` tool starts an agent on
a task whenever your user id is listed in `MUSTER_CHIEF_DISPATCH_OWNERS`. So,
until #824 ships:

- **Do not list yourself in `MUSTER_CHIEF_DISPATCH_OWNERS`** on the muster that
  this plugin files into.
- **Do not start an agent on these tasks by hand** either: not from a host
  (the host spawn), and not with `POST /api/agents` and the task's `note_id`.

Once a release with #824 is out, the first message an agent gets about the
task is fenced. Its tools may still return the text unfenced: `task_get`
(and `GET /api/tasks`) return the task as stored, on releases without a
follow-up to #824 that fences them too.
<!-- TODO: tighten this section once muster #824 merges: name the release that carries it, and whether task_get / GET /api/tasks are fenced in it. -->

This plugin has no auto-start switch of its own. muster PR
[#821](https://github.com/ZacxDev/muster/pull/821) would add per-plugin
auto-start on the plugin's card on `/plugins`; it is not merged, so no released
muster has that switch.

On a repository where strangers can open issues, also set `GITHUB_LABELS` so
only issues a maintainer has labelled are filed.

## Configuration

Read from the environment only; `.env.example` lists them.

| variable | required | meaning |
|---|---|---|
| `MUSTER_URL` | yes | muster's address. `https://`, or `http://` to `localhost`, `127.0.0.1` or `[::1]` only; the address alone |
| `MUSTER_PLUGIN_TOKEN` | yes | the `mpl_…` token from `/plugins`. Never logged |
| `GITHUB_TOKEN` | yes | a fine-grained token with **Issues: Read-only** on the repositories. Never logged |
| `GITHUB_REPOS` | yes | `owner/name`, comma-separated |
| `GITHUB_LABELS` | no | only issues with **at least one** of these labels (comma-separated, case-insensitive). Empty: every issue |
| `GITHUB_POLL_INTERVAL` | no | a Go duration of at least `10s`; default `1m` |
| `GITHUB_SINCE` | no | RFC 3339; where a repository with no cursor yet starts. Default: the first start's time |
| `GITHUB_TASK_NAMESPACE` | no | the task key namespace, default `github`. It must be in the server's `MUSTER_PLUGIN_TASK_NAMESPACES` |
| `GITHUB_API_URL` | no | default `https://api.github.com`; GitHub Enterprise Server is `https://<host>/api/v3` |
| `GITHUB_MAX_PER_HOUR` | no | how many task requests an hour the plugin sends muster (1-3600), default `60`. Events past it wait in the outbox |
| `GITHUB_BURST` | no | how many it may send at once before that pace applies (1-1000), default `10` |
| `STATE_DIR` | no | where the outbox and the cursors live; default `./state` (the Docker image sets `/state`: leave it unset there) |

## What it files

| on GitHub | in muster (`POST /plugin/v1/tasks`) |
|---|---|
| an open issue it has not filed (new, or newly carrying a filter label) | a task: title `owner/repo#n: <title>`; body = the issue's link, repository, number, author and labels first, then the issue's text, trimmed to about 15 KiB in all with a `[trimmed: the full text is on GitHub]` marker; `route_to_chief` with `notice: assigned` |
| an issue reopened after it saw it closed | the same (so a task completed meanwhile is filed again) |
| any other change to a filed issue (edit, comment, label, close), **even once its filter label is gone** | a comment on that task: the link, the state, the labels and the newest 5 of the GitHub comments since the last change it recorded (2,000 bytes each); `notice: update` (muster passes it on while an agent works the task, else files it only) |
| a pull request, an issue it has not filed that has no filter label, an issue first seen closed | nothing |

The label filter decides what gets **filed**. Once an issue is filed, taking
the label off does not cut its task off from later changes such as the close.

The body puts the link first because muster keeps at most 16 KiB of a task
body and cuts from the end.

The task's key is `github:<owner>/<repo>/<n>` (lower-case; a repository name
too long for muster's 64-character tag becomes 12 hex characters of its
sha256). muster keeps one open task per key.

**Nothing is filed twice.** Each issue change is one event, named
`<key>@<updated_at>`. Every comment starts with that name in brackets, because
muster adds a comment once per text and names the routed event by key and
comment. And the plugin writes each event to `STATE_DIR/outbox.jsonl`
(fsynced) before it moves its cursor. It refuses an event name the outbox
already holds, so a re-polled issue (GitHub's `since` is inclusive) never
becomes a second request. The outbox also stores the exact request, so a
retry sends the same bytes even if the issue has changed since.

This plugin files **copy** tasks (`create` and `comment`), not task
references, so it sends no `event_id` (muster accepts that field only on a
reference).

## What it does on each answer

| from | answer | the plugin |
|---|---|---|
| muster | `201` / `200` | marks the event delivered |
| muster | `400` | logs it and drops it: the same request can never be accepted |
| muster | `409` | logs it and marks it delivered: the task is filed, but muster will not route this event, and sending it again cannot help |
| muster | `429` | waits `Retry-After`, then sends the **same** request again |
| muster | `503`, other `5xx`, no answer | waits (`Retry-After` if given, else 5 s doubling to 5 min) and sends it again; the event stays in the outbox, so a restart keeps it |
| muster | `401`, `403`, `404` | stops: the token is revoked or lacks `messages`, or plugins are off on that server |
| GitHub | rate limit (`403`/`429` with `Retry-After` or `x-ratelimit-remaining: 0`) | waits until GitHub's reset (at most an hour), then polls again from the same cursor |
| GitHub | `401` | stops: the token was revoked or has expired |
| GitHub | anything else (its text cut to 2 KiB in the log) | logs it and tries again next poll; the cursor stays at the last page fully recorded |

At startup it checks that the token can see each repository and refuses to
start if one is missing. It also refuses to start when muster answers `400`
to the namespace check (the namespace is not in `MUSTER_PLUGIN_TASK_NAMESPACES`).

**Paging.** GitHub's issue list is sorted by `updated_at`, and its `page=2`
link is an offset into that list. An issue updated while a poll is between
pages moves to the end, and the issue that shifts across the page boundary
would never be answered. So the plugin does not walk page links: after each
page it asks again with `since=` the newest `updated_at` it has recorded. It
follows the page link only when a whole page shares one `updated_at`, where
`since=` cannot move on.

## Token scope

- **GitHub**: a fine-grained token with **Issues: Read-only** (and the
  Metadata: Read-only GitHub adds) on the repositories you list. The plugin
  sends only `GET` requests: the repository, its issues, an issue's comments.
- **muster**: the plugin token's default `messages` capability. It opens
  muster's `/plugin/v1/*` routes and nothing else, acting as you. This plugin
  calls three of them: `GET /plugin/v1/self` and `GET /plugin/v1/tasks` (once
  each, at startup) and `POST /plugin/v1/tasks`.
  Revoke it on `/plugins` at any time; the plugin then stops at its next call.

## Telemetry: none

The plugin talks only to the GitHub API at `GITHUB_API_URL` and the muster at
`MUSTER_URL`.

## State

`STATE_DIR` holds `outbox.jsonl` (each event and whether it was delivered;
the ids of delivered events are kept 30 days, then dropped when the plugin
starts) and `github-state.json` (each repository's cursor and the last state of
each issue it filed). Keep the directory between runs. If you delete it, the
next start begins again at `GITHUB_SINCE` or the current time.

An issue that is **closed** is dropped from `github-state.json` 90 days after
its last update, so the file does not grow forever. If such an issue is
reopened later, it is filed as a new issue.

## Docker

```sh
docker build -t muster-github-issues .
docker run -d --env-file .env -v muster-github-issues-state:/state muster-github-issues
```

The image sets `STATE_DIR=/state` and declares `/state` a volume. **Do not set
`STATE_DIR` in the `.env` you pass with `--env-file`**: it would override the
image's value, and the outbox and cursors would be written inside the
container instead of the volume and lost with it. `.env.example` leaves it
commented out for that reason.

`MUSTER_URL` must be reachable from inside the container. `http://localhost`
there is the container itself, not your host. Use muster's `https://` address,
or run with `--network host` on Linux to reach a muster on the host's loopback.

## Not done yet

- **Writing back to GitHub** (a comment on the issue when its task moves).
  muster's queued task writes (`POST /plugin/v1/task-writes/claim`) exist only
  for task **references**. A reference sends *every* status change and comment
  written on the task in muster to the plugin, to apply upstream. That needs a
  token with Issues: Read and write, and a mapping of muster's four statuses
  onto GitHub's two. Until a plugin applies a write, the task cannot move in
  muster, so an agent working it could not set its own status. This plugin
  keeps the read-only token and files copies.
- **Webhooks** instead of polling (needs a public URL).
- **Marking edited comments.** GitHub's `since=` on an issue's comments
  filters on `updated_at`, so an older comment edited since the last change
  the plugin recorded is quoted again, under its original creation time and
  with nothing to say it was edited.

## Layout

| path | what |
|---|---|
| `main.go` | reads the environment, starts the two loops |
| `internal/poller` | polls GitHub and turns issue changes into events |
| `internal/github` | the GitHub REST client (GET only) |
| `internal/githubtest` | a fake GitHub for tests (`httptest`) |
| `internal/outbox` | the durable event outbox: the dedupe, and delivery across restarts |
| `internal/deliver` | delivers the outbox to muster, with the retry rules above |
| `internal/muster` | the client for muster's plugin routes |
| `internal/mustertest` | a fake muster for tests (`httptest`), answering as muster does |

## Tests

```sh
go test -race ./...
```

They run against the fake muster and the fake GitHub and cover: muster down,
`429`, `503`, `409`, `400`, a revoked token, a duplicate event, a restart in
the middle of a batch and before the state is saved, a poll cut off by a rate
limit, an issue updated between pages, a page sharing one `updated_at`,
labels (including a label removed from a filed issue), pull requests, close
and reopen, a long body, the newest comments across pages, forgetting closed
issues, the namespace check, the delivery rate limit, byte-identical retries,
and that no request carries `event_id`. CI runs gofmt, `go vet`, `go test -race`,
`govulncheck` and gitleaks (history and tree, `.gitleaks.toml`).

## Licence

MIT. See [LICENSE](LICENSE).
