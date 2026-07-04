# Publishing D BOX & shipping updates

You do **not** need a server. The website, the download, and the one-click
auto-update all run off free static hosting. Two homes:

| Thing | Where it lives | Cost |
|-------|----------------|------|
| The installer + raw `.exe` (big files) | **GitHub Releases** (release assets) | free |
| The landing page + `latest.json` | **GitHub Pages** (or Netlify) | free |

The app checks `latest.json` on startup, compares its version, and — one click —
downloads the new `.exe`, swaps itself, and restarts. That's the whole update
system: one small static JSON file you overwrite each release.

---

## One-time setup

1. **Make a GitHub repo** named `dbox` (public). Push this project to it.

2. **Edit two placeholders** to your GitHub username:
   - `scripts/release.sh` → `GITHUB_USER`, `GITHUB_REPO`
   - `website/index.html` → the `ghLink` href (cosmetic)

   The manifest URL becomes `https://<user>.github.io/dbox/latest.json`. If you
   later use a custom domain / Netlify, change `MANIFEST_URL` in `release.sh` to
   that and re-release.

3. **Turn on GitHub Pages**: repo → Settings → Pages → Source = *Deploy from a
   branch*, Branch = `main`, Folder = `/website`. Save. Your site is now at
   `https://<user>.github.io/dbox/`.

4. Install the [GitHub CLI](https://cli.github.com/) (`gh`) once and `gh auth login`
   — the release script uses it to upload assets.

---

## Cutting a release (every new version)

```bash
scripts/release.sh 1.1.0
```

That builds `dist/DBox.exe`, `dist/DBox-Setup-1.1.0.exe`, and `dist/latest.json`
(with the version **baked into the exe** and the sha256 of the exe in the
manifest). Optionally write release notes to `dist/notes-1.1.0.txt` first — they
land in the manifest and show in the app's "Update available" dialog.

Then two uploads:

```bash
# 1) attach the binaries to a GitHub Release tagged v1.1.0
gh release create v1.1.0 dist/DBox.exe "dist/DBox-Setup-1.1.0.exe" \
    -t "D BOX v1.1.0" -n "$(cat dist/notes-1.1.0.txt 2>/dev/null)"

# 2) publish the updated latest.json (release.sh already copied it to website/)
git add website/latest.json && git commit -m "release v1.1.0" && git push
```

Within a minute, every running copy of D BOX sees the new `latest.json`, and on
next launch (or Help → Check for updates) users get the one-click prompt.

---

## How the version is wired

- `internal/version/version.go` holds `Version` and `ManifestURL`. A plain
  `go build` leaves them `dev`/empty, which **disables** update checks (so your
  dev builds never self-update). `release.sh` stamps both via `-ldflags -X`.
- `VERSION` (repo root) is the source number the script reads. Bump it by
  passing the version to `release.sh`.
- The installer picks up the same number: `ISCC /DMyAppVersion=1.1.0`.

## Notes

- **SmartScreen**: unsigned apps show "Windows protected your PC" on first run
  (and sometimes after an update). Users click *More info → Run anyway*. A
  code-signing certificate (~$100-200/yr, e.g. from Sectigo/DigiCert, or free
  via [SignPath](https://signpath.io) for OSS) removes it. Skippable at first.
- **Only DBox.exe is swapped** by the in-app updater (the bundled `yt-dlp.exe`
  keeps itself current, and `aria2c.exe` rarely changes). If you ever need to
  update those too, point users at the installer for that release.
- **latest.json is the single control point.** To roll back, just re-point it at
  the previous version's URLs. To do a staged rollout you'd need a real backend —
  not required here.
- **CDN caching**: GitHub Pages/Release URLs are already CDN-backed. The app and
  site both request `latest.json` with `Cache-Control: no-cache` so a new release
  is picked up promptly.
