# Arrmada Autoscan for Silo

A [Silo](https://github.com/Silo-Server/silo-server) plugin that scans what
[Arrmada](https://github.com/tristenlammi/Arrmada) changes. It implements
Silo's `scan_source.v1` capability, the one the first-party
[Sonarr and Radarr plugin](https://github.com/Silo-Server/silo-plugin-autoscan-arr)
implements, against Arrmada's own API.

> **Draft.** Arrmada 0.x answers a personal API key with 403 on every route
> outside its key allowlist (`internal/httpapi/apikey_routes.go`), and the
> three routes below are not on it. Until they are, a poll fails with
> "the API key's account is not a manager or admin (HTTP 403)".

## What it scans

Silo calls `PollChanges` on its Autoscan timer (600 seconds by default). The
plugin then:

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

## Marker

The marker is `v1;m=<movie event id>;s=<series event id>`: the newest event of
each kind already reported. Arrmada's event ids only grow, so the plugin never
compares clocks. The first poll of a source stores the newest ids and reports
nothing. A marker the plugin cannot read, or ids lower than the marker (an
Arrmada database restored from a backup), restart from the newest event in the
same way.

## Setup

1. In Arrmada, under Me, Personal API keys, create a key for a manager or
   admin account. A read-only key is enough: the plugin only sends `GET`.
2. In Silo, under Admin, Plugins, upload the binary for the server's platform.
3. Under Admin, Libraries, Autoscan, add a connection with Arrmada's address
   (`http://arrmada:7878`, or wherever Silo reaches it) and the key. Silo's
   form offers the Sonarr and Radarr types only; pick either, it is a label.
   Test connection calls the Sonarr API and fails against Arrmada.
4. Add a source, choose **Arrmada**, and bind the connection.
5. When Arrmada and Silo mount the media at different paths (Arrmada's
   `/media/movies` is Silo's `/data/movies`, say), add a path rewrite on the
   source.

The plugin stores nothing. The address and key arrive with each poll, and the
key is sent in the `X-Api-Key` header; Arrmada refuses a key in the address.

## Building

```sh
make build        # ./plugin for this platform
make build-all    # dist/plugin-<os>-<arch> for linux/amd64, linux/arm64, darwin/arm64
make test         # unit tests
make e2e          # builds the binary and polls it over go-plugin gRPC, as Silo does
./plugin manifest # the manifest Silo reads from the binary
```

The version in the manifest comes from `git describe`, or `VERSION=1.2.3 make build`.

## License

MIT
