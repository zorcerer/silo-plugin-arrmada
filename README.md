# Arrmada for Silo

A [Silo](https://github.com/Silo-Server/silo-server) plugin that connects it to
[Arrmada](https://github.com/tristenlammi/Arrmada), with two capabilities:

- **Requests** (`request_router.v1`): a movie or series requested in Silo
  becomes a request in Arrmada, and Silo shows its progress.
- **Autoscan** (`scan_source.v1`): what Arrmada imports, upgrades, renames or
  deletes is scanned into Silo's library.

They take different credentials. Arrmada's personal API keys reach its
requests routes but not its history or library, so Autoscan signs in with an
account.

## Network access and data

- The plugin exposes no network service: it declares no HTTP routes and
  opens no port. Silo starts it as a child process and talks to it over a
  Unix domain socket that go-plugin creates in a temporary directory.
- The plugin connects to one address: the Arrmada address on each Silo
  connection. It makes no other outbound connections and sends no telemetry.
- It stores nothing on disk. Silo stores the address and credential
  (encrypted) and sends them with every call.
- Requests send Arrmada the title's TMDB id, title, year, seasons and the
  note "Requested in Silo by <user>", where `<user>` is the Silo username of
  the requester.
- Autoscan reads Arrmada's history and the file paths of the titles in it,
  and returns those paths to Silo.
- Autoscan's account session cookie is held in memory for the life of the
  plugin process. The password is never written to a log or an error.
- Requests use the `X-Api-Key` header; Autoscan signs in with
  `POST /api/v1/auth/login`. Both send the User-Agent `silo-plugin-arrmada`.
- It does not follow redirects, verifies TLS certificates on `https://`
  addresses, and reads at most 16 MiB of any answer.
- Over `http://`, the API key and the Autoscan password cross the network
  unencrypted. On a Docker network between two containers on one host that
  stays inside the host; across machines, use `https://`.
- The Arrmada address must be `http://` or `https://` with a host, and
  without credentials, a query or a fragment.
- Only absolute paths without control characters are passed to Silo, which
  scans a path only inside one of its library folders.

## Compatibility

Built against [`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk)
v0.32.1 and Silo API `v1`, for linux/amd64, linux/arm64 and darwin/arm64.
Arrmada 0.3.1 or later, the first release with personal API keys.

### Tested on a real server

On Silo `build-2249` with Arrmada 0.4.0, the maintainer has tested:

- Requests, with plugin v0.1.1: the request server's connection test, and
  creating requests from Silo, a series among them.
- Autoscan, with a v0.2 release: signing in with an email and password
  account, and a title Arrmada imported being scanned into Silo's library
  through the source's path rewrite (`/storage` to `/mnt/user/data`).

Everything else (season-scoped requests from v0.2.0, approval, 4K, download
progress, the error messages, and the v0.2.2 changes) has so far been run
only by the test suite, against stand-ins for Silo and Arrmada built from
their source.

A Silo server running in Docker reaches Arrmada by its container name on a
shared Docker network (`http://Arrmada:7878`). A reverse proxy in front of
Arrmada that admits only some source addresses answers the Silo container
with 403; the connection test says so.

## Install

In Silo, under Admin, Plugins, add a repository with this URL, then install
**Arrmada** from it:

```
https://github.com/zorcerer/silo-plugin-arrmada/releases/latest/download/index.json
```

Every release attaches its own `index.json`, so that URL always names the
newest release. The binaries carry a build provenance attestation
(`plugin.sigstore.json`); Silo checks it and records the result, and installs
either way.

To install by hand instead, upload the `plugin-<os>-<arch>` for the Silo
server's platform from a release.

## Requests

### Setup

1. In Arrmada, under Me, Personal API keys, create a key with **Full** access
   for a manager or admin account.
2. In Silo, add a request server for the **Arrmada** capability with Arrmada's
   address (`http://arrmada:7878`, or wherever Silo reaches it) and the key.
   Test connection says which account and role the key acts as.

### What it does

- **Fulfill** posts `POST /api/v1/requests` with the title's TMDB id, a
  show's seasons as Silo asked for them, and the note "Requested in Silo by
  <user>". The plugin declares `supports_seasons`, so Silo sends the season
  list, and for a show it already has, only the seasons it is missing.
  Arrmada leaves out specials and seasons its catalogue does not list,
  requests only the seasons no other request or the library covers, and
  follows the existing request when all of them are covered. When the
  request comes back pending and **Approve requests** is on (the default), it
  approves it with `POST /api/v1/requests/{id}/approve`. That needs a manager
  or admin key with full access; with any other key the request waits in
  Arrmada for staff, and Silo shows it as "Awaiting approval".
- Arrmada keeps one request per title. Of the qualities Silo asks for, the
  first HD one is sent with the **Quality profile** field. A 4K quality is sent,
  with the **4K quality profile** field, only when **Send 4K requests** is on and
  no HD quality was asked for. An empty profile is Arrmada's default.
- **CheckStatus** reads `GET /api/v1/requests/{id}` and maps Arrmada's stage:

  | Arrmada | Silo | Download phase |
  |---|---|---|
  | pending (not approved) | queued, "Awaiting approval" | |
  | searching | queued | |
  | queued, downloading, paused | downloading | the same |
  | failed (Arrmada retries with another release) | downloading | stalled |
  | importing, adding | downloading | importing |
  | partial (some episodes) | downloading | |
  | available | completed | |
  | declined, or the request deleted in Arrmada | failed | |

  While a download runs, Silo gets its size, what is left and the expected
  finish, and checks every minute.
- A title already in Arrmada's library is completed at once. A refusal from
  Arrmada that will repeat (a quota, a title it does not take, seasons it
  does not know) fails the target with Arrmada's message. Arrmada
  unreachable, a 5xx, or a refused key is returned to Silo as an error, so
  Silo tries the request again later.
- An approval that fails for a reason that may pass is retried the same way;
  Arrmada answers the repeated request with the one already made. An
  approval Arrmada refuses for good (an unknown quality profile) leaves the
  request pending, with the reason in its message.
- The plugin does not follow redirects. An address that redirects is
  reported, with the host it redirects to.

The quality profile is a text field: Arrmada's keys cannot list its profiles.

## Autoscan

### Setup

1. In Arrmada, use a manager or admin account **without two-factor sign-in**,
   with a password: an account that signs in only with Plex or Jellyfin has
   none. A dedicated account (say `silo@example.com`) keeps its session apart
   from yours. A manager account is never made to set up two-factor; an admin
   is, when "Require two-factor for admins" is on.
2. In Silo, under Admin, Libraries, Autoscan, add a connection:
   - **Kind:** Sonarr or Radarr. Silo offers only these; the plugin ignores
     which.
   - **Base URL:** Arrmada's address, such as `http://arrmada:7878`.
   - **API key:** the account's sign-in name, a colon, and its password:
     `silo@example.com:password`. The sign-in name is what Arrmada's sign-in
     page asks for: an email address on an Arrmada that signs in by email,
     otherwise the username. The value is split at the first colon, so the
     password may hold one. Silo stores it encrypted.
3. **Ignore Test connection on this form.** Silo runs that test itself
   against the Sonarr API, not through the plugin, so against Arrmada it
   always shows "Connection could not be verified. Check its configuration
   and access." whatever the credentials. Click **Save changes**; the test
   does not block saving.
4. Add a source, choose **Arrmada**, and bind the connection.
5. When Arrmada and Silo mount the media at different paths (Arrmada's
   `/storage` is Silo's `/mnt/user/data`, say), add a path rewrite on the
   source.
6. Check it in the source's activity log after the next poll (lower the
   Autoscan poll interval to see it sooner). The first poll succeeds with 0
   changes: it only records where Arrmada's history is. A failure shows the
   plugin's own message: a refused password, two-factor sign-in, an address
   that is not Arrmada, or an API key in place of an account.

The plugin signs in with `POST /api/v1/auth/login` and keeps the
`arrmada_session` cookie in memory for the life of the plugin process, so the
account's device list holds one session per process. When Arrmada answers 401
it signs in once more. An account with two-factor sign-in fails with a message
saying so.

### What it scans

On each poll (600 seconds by default) the plugin:

1. Reads `GET /api/v1/history?kind=movie` and `?kind=series` with
   `event=all`, newest first, 200 events a page, until it reaches the last
   event it reported, at most 25 pages (5,000 events) per kind per poll. Of
   those it keeps the events that change files: `imported`, `upgraded`,
   `renamed`, `renumbered`, `deleted`, `file.deleted`, `version_added`,
   `version_removed`, `moved` (to another library folder) and
   `files.repaired`. History's own categories file the last three under
   "other", so the plugin picks them out by name.
2. Looks up each title those events name, oldest event first:
   `GET /api/v1/movies/{id}` and `GET /api/v1/series/{id}`. After 2 minutes
   it stops and returns what it has; the next poll carries on from the first
   event it did not reach. Silo gives a poll 5 minutes. A lookup that fails in
   a way that may pass (Arrmada unreachable, a 5xx) also stops the poll there.
   A title whose answer cannot be read is skipped.
3. Returns, as Arrmada sees the paths:
   - for a movie, each of its files (every version) as a `FILE` change. Silo
     scans the file's folder, which also picks up a file a rename or upgrade
     replaced;
   - for a show, its folder as a `SUBTREE` change. Arrmada's history says how
     many episodes an import placed but not which, so the whole show is
     scanned. The show folder is the folder that holds every episode file's
     folder, when each of those is that folder or a season folder directly in
     it. When the files are spread further apart (Arrmada calls them
     misplaced), each folder is scanned on its own, so a TV root is never
     scanned whole.

The plugin reports where a title's files are now, and keeps no record of
where they were. So:

- a title removed from Arrmada, a movie whose last file was deleted, or a show
  whose last episode file was deleted has no path left to report;
- a title moved to another library folder is scanned in its new folder only;
- a season folder emptied by a delete is not scanned.

Silo's next full scan of the library marks what is gone as missing.

Books, audiobooks and music are not read yet.

### Marker

The marker is `v1;m=<movie event id>;s=<series event id>`: the newest event of
each kind already reported. Arrmada's event ids only grow, so the plugin never
compares clocks. The first poll of a source stores the newest ids and reports
nothing. A marker the plugin cannot read, or ids lower than the marker (an
Arrmada database restored from a backup), restart from the newest event in the
same way.

## Building

```sh
make build        # ./plugin for this platform
make build-all    # dist/plugin-<os>-<arch> for linux/amd64, linux/arm64, darwin/arm64
make test         # unit tests
make e2e          # builds the binary and calls it over go-plugin gRPC, as Silo does
./plugin manifest # the manifest Silo reads from the binary
```

The version is `manifest.json`'s, which a release tag must match. `go run ./cmd/index -version 1.2.3 -dist dist`
writes the repository index for binaries in `dist/`.

## Support

Use [GitHub Issues](https://github.com/zorcerer/silo-plugin-arrmada/issues)
for questions and bug reports. Report security problems privately, as
[`SECURITY.md`](SECURITY.md) describes.

## Releases

Releases are built and signed by Silo's shared plugin release workflow.
[CONTRIBUTING.md](CONTRIBUTING.md#release) describes how a release is cut and
how to verify a downloaded binary.

## AI disclosure

- Harness: Claude Code 2.1.311
- Tool(s): Claude Code CLI; govulncheck v1.9.0 for the dependency scan
- Model(s): `claude-opus-5-5`, as the session reports it; all of the
  plugin's code was written in that one session
- Involvement: Fully AI-generated, human verified. The maintainer (zorcerer)
  directed the work and tested what [Tested on a real server](#tested-on-a-real-server)
  lists.
- Adversarial review: two passes with Claude Code's `/code-review` (high
  effort), one over the disclosure and documentation and one over the Go
  source against the Silo SDK, Silo server and Arrmada source. They found
  wrong claims in this section, a release error path that marked temporary
  failures as permanent, Autoscan polls that could time out on the same
  window forever, event names it missed, credentials that would follow a
  redirect to another host, and other services' response bodies shown in
  error messages. Then a security review by the model, covering what the
  plugin exposes, credential handling, the address an admin enters, size
  limits, TLS, injection, the paths reported to Silo and the workflows,
  with govulncheck. It found nothing exposed (no routes or ports, only the
  Unix socket to Silo), but releases built with Go 1.26.0 and
  golang.org/x/net 0.58.0, both with known vulnerabilities reachable from
  the plugin, and an address holding user:password repeated in errors. The
  fixes are in v0.2.2, built with Go 1.26.11; govulncheck now runs in CI.
  Not fixed: deleted files leave no path to report (see Autoscan above).

Pull requests from #11 on end with the same disclosure for their own changes;
`.github/PULL_REQUEST_TEMPLATE.md` carries the fields.

## License

MIT
