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

## Compatibility

Built against [`silo-plugin-sdk`](https://github.com/Silo-Server/silo-plugin-sdk)
v0.32.1 and Silo API `v1`, for linux/amd64, linux/arm64 and darwin/arm64.
Arrmada 0.3.1 or later, the first release with personal API keys. Requests
have been used with Arrmada 0.4.0 and a Silo `build-2249` server.

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
  Arrmada (a quota, a title it does not take) fails the target with Arrmada's
  message.

The quality profile is a text field: Arrmada's keys cannot list its profiles.

## Autoscan

### Setup

1. In Arrmada, use a manager or admin account **without two-factor sign-in**.
   A dedicated account (say `silo`) keeps its session apart from yours.
2. In Silo, under Admin, Libraries, Autoscan, add a connection with Arrmada's
   address, and `username:password` in the API key field (Silo stores it
   encrypted). The value is split at the first colon, so the password may hold
   one. Silo's form offers the Sonarr and Radarr types only; pick either, it is
   a label. Test connection calls the Sonarr API and fails against Arrmada.
3. Add a source, choose **Arrmada**, and bind the connection.
4. When Arrmada and Silo mount the media at different paths (Arrmada's
   `/media/movies` is Silo's `/data/movies`, say), add a path rewrite on the
   source.

The plugin signs in with `POST /api/v1/auth/login` and keeps the
`arrmada_session` cookie in memory for the life of the plugin process, so the
account's device list holds one session per process. When Arrmada answers 401
it signs in once more. An account with two-factor sign-in fails with a message
saying so.

### What it scans

On each poll (600 seconds by default) the plugin:

1. Reads `GET /api/v1/history?kind=movie` and `?kind=series`, filtered to the
   `imported`, `upgraded`, `renamed` and `deleted` categories, newest first,
   200 events a page, until it reaches the last event it reported. It reads at
   most 25 pages (5,000 events) per kind per poll.
2. Looks up each title those events name:
   `GET /api/v1/movies/{id}` and `GET /api/v1/series/{id}`.
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

A title removed from Arrmada, or a movie whose last file was deleted, has no
path left to report. Silo's next full scan of the library marks it missing.

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

## License

MIT
