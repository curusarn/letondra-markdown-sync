# Letondra Markdown Sync

A small Windows app that keeps Google Drive folders and Obsidian folders in two-way sync.

- On Google Drive the notes are **Google Docs**, editable anywhere (web, phone).
- In Obsidian they are **plain `.md` files**.
- Start the app and it opens Obsidian and syncs while it runs: every save in Obsidian goes to Drive within seconds. Edits made on Drive come down within the poll interval (30 s), and immediately when you come back to the computer or it wakes from sleep.
- Edits to different parts of the same note made on both sides are merged automatically, like git does. If both sides changed the same lines, a page opens in the browser with the two versions side by side. You pick left, right or both, or edit the result.

Only `.md` files sync. Images and other attachments don't.

## Download

Every push to `main` builds the app and publishes it on the [Releases page](https://github.com/curusarn/letondra-markdown-sync/releases). Download `letondra-markdown-sync.exe` from the newest release. It is a single file and needs no installation.

## First-time setup

You need two Google credentials, both from the Google Cloud project `letondra-gugl-klaud`:

1. **The service account key** (JSON). The service account (`automat@letondra-gugl-klaud.iam.gserviceaccount.com`) edits existing documents and watches for changes.
   - **Share every Drive folder you want to sync with it, as Editor.**
2. **An OAuth client of type "Desktop app"** (JSON). With it the app signs in once with your own Google account and creates new notes on Drive. The service account can't create files, because it has no storage.

   One-time setup in the [Google Cloud Console](https://console.cloud.google.com/auth/overview?project=letondra-gugl-klaud):
   1. Google Auth Platform → Branding: app name, support email. Home page, privacy policy and terms are this repository's GitHub Pages: `https://curusarn.github.io/letondra-markdown-sync/`, `…/privacy.html`, `…/terms.html`. Authorised domain: `curusarn.github.io`.
   2. Audience: User type **External**. Add your own Google account as a test user.
   3. Clients → Create client → Application type **Desktop app** → download the JSON.
   4. **Recommended:** Audience → **Publish app** ("In production"). While the app is in "Testing", Google expires the sign-in after 7 days and you have to sign in again. In production it stays signed in. A personal app doesn't need Google's verification; the sign-in screen just warns that the app is unverified (Advanced → Continue).

Then:

1. Put both JSON files somewhere private, e.g. `C:\Users\<you>\Documents\markdown-sync\`.
2. Run `letondra-markdown-sync.exe`. On first start it opens its settings page in the browser:
   - the paths to the two JSON files,
   - "Spustit s aplikací": what to start with the app. The default `obsidian://open` opens Obsidian; leave it empty to start nothing.
   - folder pairs: a local folder (full path) and the Drive folder link, optionally with excluded subfolders.
3. Save. On the overview page click **Přihlásit se Googlem** and sign in with your account.

The app lives in the notification area (tray). The icon colour shows the state:

| Colour | Meaning |
|---|---|
| Green | Synced |
| Orange | A conflict is waiting |
| Grey | Offline, retrying |
| Red | Something needs you: settings, sign-in or an error. Open the overview. |

Click the icon to open the overview.

## Files the app keeps

Everything is in `%APPDATA%\LetondraMarkdownSync\`:

| File | What |
|---|---|
| `config.json` | settings |
| `state.json`, `blobs\` | which Drive document is which file, and the last synced version of both sides |
| `google-token.bin` | your Google sign-in, encrypted for your Windows user |
| `app.log` | what the app did |
| `backups\` | every local file before the app overwrote or removed it, kept for 30 days |

Drive keeps its own version history of each document.

## When something goes wrong

Start with the overview page (tray icon → Otevřít přehled), then `app.log`. The full troubleshooting list is in [CLAUDE.md](CLAUDE.md#when-something-breaks).

## Development

The code is Go. It builds and tests on any OS, and the `.exe` is cross-compiled:

```sh
go test ./...
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" -o letondra-markdown-sync.exe ./cmd/letondra-markdown-sync
```

Read [CLAUDE.md](CLAUDE.md) before changing the sync logic.
