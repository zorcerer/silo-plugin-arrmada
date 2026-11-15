# Contributing to Arrmada for Silo

Open an issue before a large change. Pull requests go against `dev`.

## AI disclosure

Every pull request ends with the AI disclosure block from
[`.github/PULL_REQUEST_TEMPLATE.md`](.github/PULL_REQUEST_TEMPLATE.md), in the
format of Silo's
[AI-assisted contribution policy](https://github.com/Silo-Server/silo-server/blob/main/docs/ai-contributions.md).
"No AI used" is a complete answer when true.

## Checks

Run these before opening a pull request; CI runs the same:

```sh
make vet test e2e build-all
```

`go.mod` must not replace `silo-plugin-sdk` or point a module at a local
path. For work across repositories, use a local `go.work` and build with
`GOWORK=off` before pushing.

## Release

Maintainers cut releases. Pushing a `v<semver>` tag runs
[`release.yml`](.github/workflows/release.yml), which calls Silo's shared
`Silo-Server/.github` plugin release workflow. It builds the binaries, signs
them with a GitHub artifact attestation, publishes the GitHub release, and
notifies the [community catalog](https://github.com/Silo-Community/silo-plugins)
when the repository has the `SILO_PLUGINS_DISPATCH_TOKEN` secret. A second job
then attaches `index.json`, the Silo plugin repository index.

1. Open a pull request that sets `version` in `manifest.json` to the new
   version, and merge it into `dev`, then `dev` into `main`.
2. Tag the commit that put the new version on `main` and push the tag:

   ```sh
   git switch main && git pull --ff-only
   git tag -a v0.2.2 -m v0.2.2 <commit on main that changed manifest.json>
   git push origin v0.2.2
   ```

The shared workflow fails without publishing anything if the tag is not
`v<semver>`, `manifest.json` `version` does not match it, or the tagged commit
is not the commit on `main`'s first-parent history that raised `version`. Do
not move or delete a pushed tag; fix `manifest.json` in a new pull request and
tag the next version.

Each release carries `plugin-<os>-<arch>`, `checksums.txt`,
`plugin.sigstore.json` and `index.json`. To verify a binary:

```sh
gh attestation verify plugin-linux-amd64 \
  --repo zorcerer/silo-plugin-arrmada \
  --cert-identity https://github.com/Silo-Server/.github/.github/workflows/plugin-release.yml@refs/heads/main \
  --cert-oidc-issuer https://token.actions.githubusercontent.com \
  --source-ref refs/tags/v0.2.2
```
