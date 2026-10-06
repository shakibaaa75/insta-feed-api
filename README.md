# Instagram link sync (Go + MongoDB + WordPress)

```
Your PC (home IP)                     Your server (Render/VPS)            WordPress
  worker  ──► MongoDB Atlas ◄────────  api  ◄──── plugin (twice daily)
  (run 1-2x/day)                      GET /posts                        [ig_feed]
```

- **worker** runs on your local PC, fetches the profile, saves new post links to MongoDB.
- **api** runs on a server, reads the same MongoDB, serves `GET /posts` and `GET /status`.
- **WordPress plugin** calls the API twice a day, saves new links as `ig_post`, shows them with `[ig_feed]`.

Only links are stored. The frontend uses Instagram's own embed code.

## 1. MongoDB
Create a free cluster on MongoDB Atlas, add a database user, allow your IPs
(your home IP and your API server), and copy the connection string.

## 2. Configure
```bash
cp .env.example .env     # fill in MONGODB_URI, IG_PROFILE, API_KEY
go mod tidy              # downloads the MongoDB driver
```

## 3. Get the old posts in (first import)
Instagram returns only the latest ~12 posts to a script, so load the full history once from your browser:

1. Open the profile on instagram.com in your browser and scroll until all posts have loaded.
2. Run this bookmarklet (create a bookmark and paste it as the URL); it copies all post links:
   ```js
   javascript:(()=>{const u=[...new Set([...document.querySelectorAll('a[href*="/p/"],a[href*="/reel/"]')].map(a=>a.href.split('?')[0]))];navigator.clipboard.writeText(u.join('\n'));alert(u.length+' links copied')})()
   ```
3. Paste into `links.txt`, then:
   ```bash
   go run ./cmd/worker import links.txt
   ```
If you already have the old links from somewhere else, put them in `links.txt` the same way (one URL per line).

## 4. Test the daily check
```bash
go run ./cmd/worker run
```
Expected: `your_username: fetched 12 posts, 0 new`. If it says HTTP 401/403/429 or "login wall",
Instagram blocked that connection. Try again from another network, or set `IG_PROXY`.
A failed run never touches saved posts; it only records the error.

## 5. Run the API
```bash
go run ./cmd/api
```
Check: `curl -H "X-API-Key: YOUR_KEY" "http://localhost:8080/posts?limit=5"`

Deploy it on your server or Render with the same env vars (`MONGODB_URI`, `MONGODB_DB`,
`IG_PROFILE`, `API_KEY`). Put it behind HTTPS.

## 6. WordPress
1. Zip or copy `wordpress/ig-feed-sync.php` into `wp-content/plugins/ig-feed-sync/` and activate it.
2. Settings → IG Feed Sync: enter the API URL and API key, save, click **Sync now**.
3. Add `[ig_feed count="9"]` to any page (works in an Elementor shortcode widget).

The plugin then syncs automatically twice a day. The settings page shows the last sync result.

## 7. Schedule the worker on your PC
Build once: `go build -o worker ./cmd/worker` (Windows: `-o worker.exe`).
Run it from the project folder so it finds `.env`.

- **Windows:** Task Scheduler → two triggers: "At log on" and "Daily at 9 PM", action = `worker.exe`, start in = project folder.
- **Linux/macOS (cron):**
  ```
  @reboot sleep 60 && cd /path/to/instagram-sync && ./worker run
  0 21 * * * cd /path/to/instagram-sync && ./worker run
  ```
If the PC is off, that check is simply skipped. Only the latest ~12 posts are fetched each time, so turn the PC on at least every few days.


## Alternative: Chrome extension (use this if the worker gets HTTP 401)

Instagram now rejects script requests (`worker run` gets HTTP 401). The extension reads the
profile inside your own, normal Chrome instead, then sends the links to the API's `POST /ingest`.
The worker is not needed in this setup, but `worker import` still works.

1. Start the API (step 5) and make sure it is reachable (for a first test: `http://localhost:8080`).
2. Chrome -> `chrome://extensions` -> turn on **Developer mode** -> **Load unpacked** -> choose the `extension` folder.
3. Open the extension's **Settings** (right-click the icon -> Options): API URL, API key, Instagram username. Save and allow the permission prompt.
4. Log in to Instagram in this Chrome (the profile page works best when you are logged in).
5. Click the extension icon -> **Import all posts**. A visible Instagram tab opens, scrolls until no more posts load, and closes. The popup shows how many links were found and how many were new.
6. After that it runs by itself: while Chrome is open it checks every hour whether a sync is due, and syncs when the last success is older than 12 hours (about twice a day). After a failure it waits 6 hours before trying again. The icon shows a red `!` when the last run failed.

Notes:
- Chrome must be open around check time. If it was closed, the sync runs after Chrome starts.
- The daily check opens the profile in a background tab and reads the ~12 latest posts, which is plenty for new-post detection.
- New posts get the time they were first seen as their date. The API only adds unknown links and never changes stored posts.
- Still unofficial: Instagram can change its page layout and break the link reading. The status shown in the popup and in the WordPress settings page tells you when that happens.

## Notes
- This relies on an unofficial Instagram endpoint. It may stop working without notice and is against Instagram's terms. Saved posts stay online when it fails.
- The code was written without being compiled in my environment; run `go mod tidy` and `go vet ./...` first and fix anything they report.
