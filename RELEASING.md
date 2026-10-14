# Releasing localimage

A release publishes one set of artifacts under one version:

- npm: `localimage` (CLI launcher + Node SDK) and five platform packages `@localimage/<os>-<cpu>`
  that carry the Go binary
- PyPI: `localimage`, as five platform wheels (CLI + Python SDK, binary bundled)
- GitHub release: the raw binaries and a `SHA256SUMS` file

Everything is built by `scripts/package.py` from the single version in the `VERSION` file.

## One-time setup (needs a person)

Nothing below can be done by the CI workflow, and the workflows have not been run yet.

1. **Claim the names.** During development the package name `localimage` was unused on both npm
   and PyPI; the `@localimage` npm scope was never checked. Check all of them again:
   - npm: the package `localimage` and the scope `@localimage` (create an npm organization named
     `localimage`, or change the scope in `scripts/package.py`, `packages/npm/package.json`,
     `packages/npm/bin/localimage.js` and `scripts/bump-version.py`)
   - PyPI: the project `localimage`
2. **The repository** is `https://github.com/LocalImages/LocalImage`. The Go module path in
   `go.mod`, the package metadata (`packages/npm/package.json`, `packages/python/pyproject.toml`,
   `REPO_URL` in `scripts/package.py`) all point at it. If the repository is ever renamed or moved,
   update all of those and re-register the PyPI trusted publisher.
3. **npm token:** create an automation token with publish rights to `localimage` and
   `@localimage/*`, and store it as the repository secret `NPM_TOKEN`.
4. **PyPI trusted publishing:** in the PyPI project settings add a trusted publisher for
   owner `LocalImages`, repository `LocalImage`, workflow `release.yml`, environment `release`. No PyPI token is needed.
5. **GitHub environment `release`:** create it (Settings, Environments) and add required reviewers.
   Publishing to npm and PyPI cannot be undone, so this gives you an approval step.

## Cutting a release

1. Make sure `main` is green in CI.
2. Run the end-to-end tests locally against a fresh daemon, with Chrome installed:
   ```bash
   go build -o bin/localimage ./cmd/localimage && ./bin/localimage start &
   cd packages/npm && npm install && npm test
   ```
3. If you have a Mac or Windows machine, smoke-test the built binary there (`localimage doctor`,
   then create a session and open its live view). CI only runs Linux.
4. Set the version everywhere and commit:
   ```bash
   python3 scripts/bump-version.py 0.2.0
   git commit -am "Release 0.2.0"
   ```
5. Tag and push. The tag must equal the version:
   ```bash
   git tag v0.2.0 && git push origin main v0.2.0
   ```
6. The `Release` workflow runs the full CI, builds everything, waits for approval on the
   `release` environment, then publishes npm (platform packages first, main package last), PyPI,
   and the GitHub release.

`scripts/package.py` refuses to build if `VERSION`, `packages/npm/package.json` (including its
`optionalDependencies`) and `packages/python/pyproject.toml` disagree, and the workflow refuses a
tag that does not match `VERSION`.

## Verifying a release

In clean directories:

```bash
npm install localimage && npx localimage --version
python3 -m venv v && v/bin/pip install localimage && v/bin/localimage --version
```

Both must print the new version. Then run `localimage doctor`.

## If something goes wrong

- **npm publish failed part-way:** versions cannot be republished. Platform packages that went out
  are fine to leave; bump to the next patch version and release again.
- **A bad release is out:** `npm deprecate localimage@<version> "reason"` and yank the release on
  PyPI (Manage, Yank). Publish a fixed version. Do not rely on unpublishing.
- **Playwright version for Firefox:** `PlaywrightVersion` in `internal/session/manager.go` is baked
  into the Firefox image. When you change it, bump `firefoxRev` too (it forces a rebuild) and
  update the version in the README.
- **Changing anything under `internal/session/assets/`:** bump `chromiumRev` or `firefoxRev` so
  existing machines rebuild their cached images.

## Manual fallback

If the workflow is unavailable, from a clean checkout of the tag:

```bash
python3 scripts/package.py
for d in dist/npm/*-*/; do (cd "$d" && npm publish --access public); done
(cd dist/npm/localimage && npm publish --access public)
python3 -m pip install twine && twine upload dist/wheels/*.whl
```

## Supply chain

- Every action in the workflows is pinned to a commit (the tag is in a trailing comment), so an
  action cannot change under the publish secrets without a review. Nothing updates the pins
  automatically: bump them by hand now and then (look up the commit behind the new tag with
  `git ls-remote https://github.com/<owner>/<action> 'refs/tags/<tag>^{}'`), or add Dependabot's
  `github-actions` ecosystem later if you want pull requests for it.
- On a public repository, `npm publish` records provenance and the release binaries get a signed
  attestation (`gh attestation verify <file> --repo LocalImages/LocalImage`). Both are skipped on a
  private repository, where they would fail.

## Third-party notices

The binary bundles open-source Go modules, so every package and the GitHub release include
`THIRD_PARTY_NOTICES.md` with their licences. Regenerate it whenever `go.mod` changes:
`go install github.com/google/go-licenses@latest`, then `go-licenses report ./cmd/localimage` to review the
licences (anything that is not permissive needs a decision) and `go-licenses save` for the texts.
