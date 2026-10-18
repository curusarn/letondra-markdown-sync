# CLAUDE.md: notes for agents working on this repo

This is a personal Windows app that syncs Google Drive (Google Docs) with Obsidian (`.md` files) both ways. The requirements are in the Google Doc "Example pro automat" (Czech), which holds the full prompt and the API facts verified on 18 Oct 2026. This file explains how the code meets them, which decisions were made and why, how to test, and what to do when something breaks.

Users read Czech; the UI text is Czech. Code, comments and these docs are in English.

## Ground rules

- **Never lose a note.** Every local overwrite or removal first copies the file to `backups\` (`localfs.go: backup`). Deletions go to the Drive bin, never a hard delete. When in doubt (no base, lost access, many files gone at once), stop and ask through the UI rather than guess.
- **Never get stuck.** The only state that waits for a person is a conflict, and other files keep syncing meanwhile. Network errors, rate limits and locked files retry with backoff (2 s doubling to 5 min). Everything else shows on the overview page with what to do.
- **Never write a Google Docs export over a local file.** The export is lossy (see below). Drive changes reach the local file only as a diff between two exports (`mdconv.ApplyRemote`).
- **Test only in the test document "File 2 pro automat"** and the test folder "Markdown sync test". **Never write to "Example pro automat"**: it holds the prompt (an earlier test overwrote it once). The integration test refuses that document by name.
- Credentials never go into the repo: the service account key, the OAuth client JSON and the tokens all live in Simon's 1Password (ask the orchestrator for them). `.gitignore` blocks the usual file names.
- The repo root also serves GitHub Pages (`index.html`, `privacy.html`, `terms.html`). These are the OAuth branding links and must keep working. `.nojekyll` makes Pages serve the files as they are.

## Decisions and why

| Decision | Why |
|---|---|
| Go, one `.exe`, no installer | Cross-compiles from any OS and has no runtime to install. All logic except the tray, idle detection and DPAPI is testable on Mac/Linux. |
| Tray icon plus a local web UI (`127.0.0.1:47613`) | The simplest "simple UI" for the side-by-side conflict view and settings. The fixed port also makes it a single-instance lock: a second start opens the running instance's page. |
| Service account for edits and change detection; **Simon's own Google account (OAuth Desktop app) only to create docs and folders and to move files to the bin** | The service account has 0 B storage, so `files.create` fails with 403 `storageQuotaExceeded` (verified), and it can't trash files it doesn't own. Simon chose this on 18 Oct 2026. |
| Cheap change check: `changes.list` with a saved page token, every 30 s and right after wake or return from idle | One request when nothing changed (the engine test `TestIdlePollIsOneRequest` checks this). The service account only sees folders shared with it, so the feed is exactly "my folders". The Drive Activity API also works (`ancestorName`) but isn't needed. |
| The merge runs on markdown, with a stored base per file | Drive revisions are merged by Google (3 edits became 1 revision), so they can't be the base. `state.json` + `blobs\` keep the last synced local text and Drive export. |
| Git-like 3-way merge (`textmerge.Merge3`); edits to neighbouring lines are not a conflict | Fewer conflicts than git. An insertion at the edge of the other side's change *is* a conflict, because the order would be a guess. |
| Front matter = the block between a horizontal line at the very start of the Doc and the next horizontal line | Simon's rule (18 Oct 2026). YAML pasted as it is gets mangled by Docs. In the Doc each key is its own paragraph and lists are bullets or plain text (`tags: a, b`); both read back as a YAML list. Unchanged keys keep their exact local formatting. |
| Lines of one paragraph get a hard break on upload | Docs would join them, but Obsidian shows them on separate lines. The Doc should look like the note. |
| Escapes from the export are removed before text reaches Obsidian, except `\*` and `` \` `` and escapes at the start of a line that stop a heading, list or quote | Simon: no backslashes anywhere. The Doc never shows them (verified through the Docs API); they exist only in the export. |
| Only `.md` files sync; on Drive both Google Docs and real `.md` files (`text/markdown`) | Simon: no attachments. A real `.md` file on Drive is copied byte for byte, with no conversion. |
| First run with a file on both sides and different content: the newer one wins, the other is backed up | "Newer wins" from the brief. Drive keeps its revisions; local copies go to `backups\`. |
| Local rename = a missing tracked file plus an untracked file with identical content | Simple and safe. A note renamed *and* edited before the sync becomes "delete + new doc" (the old doc goes to the bin). |
| Mass-delete guard: more than 5 and more than half of a mapping's files gone at once → nothing is deleted until Simon confirms on the overview | A disconnected drive or an unshared folder must not empty the other side. |

## Code map

```
cmd/letondra-markdown-sync/main.go   wiring: config, UI server, engine, watchers, tray, idle/wake detection
internal/config      config.json (mappings, credential paths, launch target)
internal/state       state.json + blobs/ (content-addressed), atomic writes
internal/gdrive      Drive REST client (service account + user OAuth), desktop sign-in flow
internal/mdconv      markdown conversion: Clean (export → local), ToDoc (local → upload),
                     ApplyRemote (carry Drive edits onto the local text), front matter
internal/textmerge   Myers diff and the 3-way merge
internal/syncer      the engine: one goroutine, full scan / local reconcile / poll / per-file sync
internal/ui          web UI (overview, settings, conflict page), Czech
internal/tray        tray icon (Windows only; no-op elsewhere)
internal/platform    Windows: idle time, DPAPI, opening URLs; stubs elsewhere
internal/watch       recursive fsnotify watcher
internal/integration round trips against the real Google Docs (build tag `integration`)
```

How one file syncs (`syncer/file.go: syncFile`):

1. Fetch the Drive metadata. If it was renamed or moved on Drive, move the local file to match.
2. Local changed = hash of the file ≠ base local. Remote changed = the version differs **and** the cleaned export ≠ base export (versions also bump without content changes).
3. If only the local file changed: push. Right before the upload, check the Drive version once more; if it moved, sync again.
4. If only Drive changed: `ApplyRemote(baseLocal, baseExport, newExport)`, then write the local file, but only if it still matches what was read.
5. If both changed: `Merge3(baseLocal, local, ApplyRemote(...))`. A clean merge is written locally and pushed. Otherwise a conflict is recorded in the state and the conflict page opens.

## Google Docs markdown behaviour (verified 18 Oct 2026)

- **Export:** `GET files/{id}/export?mimeType=text/markdown`.
- **Old revisions:** `revisions.list` exportLinks with `&revision=N&exportFormat=markdown`, fetched with the bearer token.
- **Import:** `PATCH upload/drive/v3/files/{id}?uploadType=media` with `Content-Type: text/markdown` replaces the content. Docs turns it into real headings, lists, tables and checkboxes.
- **The export escapes punctuation:**
  - escaped: `\[\[link\]\]`, `\#tag`, `\!\[\[img\]\]`, `\==hl==`, `18\. 10\.`, `C:\\Users`, `https\://`, `snake\_case`, `x \* 2`;
  - bare URLs come back as `[url](url)`;
  - list items get two trailing spaces;
  - table separators become `| :---- |`.
- **Docs itself shows none of those backslashes.**
- **Lost on import:**
  - code fences (the code lines become paragraphs);
  - YAML front matter as it is, which is why the rule above exists;
  - soft line breaks, unless a line ends with two spaces.
- **Kept:** headings, bold, italics, strikethrough, inline code, links, tables, nested lists, checkboxes, footnotes, `[[wikilinks]]`, `#tags` and `==highlights==` as plain text, Czech diacritics.
- **Export is a fixed point:** export → import → export gives identical text, so after our own upload the base export is stable.
- **Bold headings:** documents created through the API export their headings as `# **Title**`; `Clean` strips the `**`.
- **Export of a Doc that starts with a horizontal line:** `---`, front matter paragraphs separated by blank lines, `---`. Two lines without a blank line between them are joined into one paragraph.
- **Service account:** it can't create files (`storageQuotaExceeded`) but can create folders in its own Drive. It edits shared files fine. In Drive Activity its own edits show as `isCurrentUser: true`.

## Testing

```sh
go test ./...        # unit tests + engine tests against an in-memory fake Drive (runs on Windows in CI)
go test -race ./...  # locally (the Windows runner has no cgo for -race)
```

The engine tests (`internal/syncer/engine_test.go`) cover:
- first download, including folders and Windows-unsafe names;
- push, pull and echo suppression;
- one request per idle poll;
- a merge of both sides;
- a conflict, its resolution, and a resolution refused because a side changed meanwhile;
- a new note waiting for the sign-in;
- renames on both sides, deletes on both sides, and a remote delete that keeps local edits;
- the mass-delete guard;
- going offline and recovering, a restart, and a lost `state.json`;
- the newer side winning on the first run;
- a raw `.md` file on Drive.

Add a test there for every bug you fix in the engine.

Round trips against the real Docs (they overwrite the test document):

```sh
# key.json: the service account key from Simon's 1Password; <id>: the ID of "File 2 pro automat"
# (list what the service account sees: GET drive/v3/files)
LMS_SA_KEY=/path/to/key.json LMS_TEST_DOC=<id> \
  go test -tags integration -v ./internal/integration/
```

They check the Doc has no stray backslashes, that local and exported lines align, that a phone edit (via the Docs API) to the body and the tags comes back exactly, the merge with a concurrent local edit, and that the export is stable.

Running the real app on Mac/Linux works too (no tray, Ctrl+C quits):

```sh
go run ./cmd/letondra-markdown-sync -dir /tmp/lms-app -no-launch -console
```

`-dir` holds `config.json`. Use the test folder for the mapping.

### End-to-end run on 18 Oct 2026 (Mac, real Drive, test folder)

The real app (`-no-launch -console`) ran against "Markdown sync test" with Simon signed in. Everything passed:
- **New notes:** a new local note and a note in a new subfolder were created on Drive, owned by Simon's account, and the folder was created too.
- **Latency:** a save showed on Drive in 5 s. A new Google Doc on Drive appeared locally in 10 s. A doc moved to the Drive bin left locally in 9 s, with a backup.
- **Edits:** a Drive edit came down with the front matter, `[[links]]`, `#tags` and the code fence intact.
- **Merge and conflict:** edits on both sides merged on both sides. A same-line edit produced a conflict; the local file stayed untouched while it waited, and the resolution reached both sides.
- **Renames and deletes:** a local rename renamed the same Drive document, a Drive rename renamed the local file, and a local delete moved the doc to the Drive bin.

**Finding: Docs edits reach Drive late.** An edit made through the Docs API was in the export at once. But the Drive metadata (`version`, `modifiedTime`), the changes feed and the Activity API only showed it about 2 minutes later, so the pull came after about 2.5 minutes. Edits through the Drive upload API show up at once. How long a human edit in the Docs app takes is being measured (see the task thread).

### Acceptance checklist on Windows

From the prompt. Do these with the real Obsidian and a real phone or browser:

1. Download the newest release `.exe` and set up two folder pairs (README).
2. An edit in Obsidian shows on Drive within 5 s.
3. An edit on the phone shows locally within 30 s while the computer is in use, and immediately after the computer was idle for 2+ minutes or woke from sleep.
4. A new Google Doc in a folder or subfolder appears locally. A new note in Obsidian appears on Drive, owned by your Google account, and later edits go to the same doc with no duplicate.
5. Edits to different parts on both sides merge. The same line on both sides opens the conflict page; after resolving, both sides are equal.
6. A note with front matter (`tags:` list), `[[links]]`, `#tags`, a code block and a callout keeps all of them locally after an edit elsewhere on Drive. Rewriting the tags on the phone as bullets or as `tags: a, b` gives a valid YAML list locally.
7. Rename and delete on each side are mirrored. Deleted files are in the Drive bin or in `backups\`.
8. Pull the network cable while editing, put the laptop to sleep, kill the process in Task Manager mid-sync: after recovery everything syncs, nothing is lost and nothing is stuck.
9. With nothing changing, `app.log` shows no uploads or downloads.

Things not yet tested on real Windows (written blind, built and vetted only): the tray menu and icon, `GetLastInputInfo` idle detection, DPAPI token encryption, opening URLs via `rundll32`, the fsnotify watcher on NTFS, and file renames while Obsidian has the note open.

## When something breaks

| Symptom | Where to look | Likely fix |
|---|---|---|
| Red icon, "Dokonči nastavení" | overview → problems list | missing or wrong JSON paths, a folder not shared with the service account, a non-absolute local path |
| "Drive složka není dostupná" | overview, `app.log` | share the folder with `automat@letondra-gugl-klaud.iam.gserviceaccount.com` as **Editor** |
| New notes don't appear on Drive, "čeká na přihlášení" | overview → Google sign-in | sign in again. If it happens every 7 days, the OAuth app is still in "Testing": publish it (README) |
| Sign-in fails with `redirect_uri_mismatch` | the OAuth client type | it must be a **Desktop app** client (loopback redirect `http://127.0.0.1:<port>/`) |
| Grey icon, "zkusím znovu" | `app.log` | network or Google outage; it retries by itself. A lasting 403 `rateLimitExceeded` means lower `poll_seconds` load |
| "Mnoho souborů zmizelo" / "Na Drive chybí" | overview | check the drive or folder is there. If the deletion is intended, confirm it on the overview |
| A note looks wrong after a pull (backslashes, joined lines, lost front matter) | `backups\` has the version before; compare with the Drive export | add the case to `mdconv` tests (`TestApplyRemote*`, `TestCleanRealExport`) with the real export, then fix `Clean` / `Unescape` / `ApplyRemote` |
| The same edit bounces back and forth, or uploads repeat | `app.log` "pushed" lines repeating | the export of our upload is not a fixed point for some construct: make `ToDoc` produce markdown whose export is stable, and add it to the integration test |
| Duplicate docs on Drive | `state.json` lost or broken (`state.json.broken-*`) | the next scan pairs by name and content. Remove the duplicate doc by hand |
| App won't start, second start does nothing | `fatal.txt`, port 47613 | another instance is running (tray), or another program holds the port |
| Anything else | `app.log`, `state.json` (file entries show `last_error`, `conflict`) | reproduce in `engine_test.go` with the fake Drive first |

To reset safely: quit the app, delete `state.json` and `blobs\`, start again. The first scan pairs files by name and content; if the content differs, the newer side wins and the other is backed up.

## Releasing

Every push to `main` runs `.github/workflows/build.yml` on `windows-latest`: vet, tests, build, then a release `v0.1.<run number>` with `letondra-markdown-sync.exe`. Simon's own repo: commit to `main` and push directly, no PRs.

## Known gaps / next steps

- A note renamed and edited before the sync runs is treated as delete + new (it loses its Drive history).
- Code blocks lose their fences in the Doc (Docs import has no code blocks). A Drive edit inside a code block comes back without the code formatting of the new lines.
- Nested list indentation from Docs exports (e.g. 6 spaces under a task item) is kept as it is in new notes from Drive.
- `poll_seconds` is fixed (30 s). It could back off while idle, but that isn't needed for cost: an idle poll is one cheap request.
