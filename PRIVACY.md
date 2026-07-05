# D BOX Integration — Privacy Policy

_Last updated: 2026-07-05_

**D BOX Integration** is the companion browser extension for the **D BOX**
desktop download manager. This policy explains exactly what the extension
accesses and where that data goes.

## The short version

**Nothing you do is sent to us or to any third party.** The extension has no
analytics, no tracking, and no remote servers. Every piece of data it touches is
sent only to the **D BOX application running on your own computer**
(`http://127.0.0.1:8081`, i.e. localhost). It never leaves your machine.

## What the extension accesses, and why

- **Downloads you start** — when you download a file, the extension reads its URL
  and filename so it can hand the download to D BOX instead of the browser's
  basic downloader.
- **Media on the page you're viewing** — the extension observes the network
  requests of the current tab to detect video/audio streams (e.g. an embedded
  player), so it can offer them to you for download. It only *observes*; it never
  blocks or modifies any request.
- **Site cookies (only when you download login-gated media)** — for sites that
  require you to be signed in (for example Instagram), the extension reads that
  site's cookies so D BOX can download content **you are already logged in to
  view**. These cookies are passed only to your local D BOX app and are used only
  to authenticate that one download.
- **Settings** — your on/off toggle and the D BOX address are stored in the
  browser's local extension storage.

## Where the data goes

Exclusively to `http://127.0.0.1:8081` — the D BOX app on your own computer.
The extension makes **no** connections to the developer or to any external
service. No data is collected, stored remotely, sold, or shared.

## Your control

- Toggle capture off any time from the extension's popup.
- Remove the extension to stop all access immediately.
- The cookie feature only activates when you choose to download from a
  login-gated site; if D BOX isn't running, nothing is sent anywhere.

## Contact

Questions or issues: open an issue at
<https://github.com/66lexel999/dbox/issues>.
