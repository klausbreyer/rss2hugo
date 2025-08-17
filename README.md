# wordpress2hugo

Small Go tool that ingests a **WordPress RSS/Atom feed** and creates **Hugo posts** — including downloading images and converting HTML to **Markdown** while **preserving the original text/image order**.

## What it does

- Robust feed parsing (gofeed) with basic XML sanitization.
- Builds the post **slug** as `YYYY-MM-title` (emojis in the slug are replaced with tokens like `u1F642`).
- Writes Hugo front matter: `title`, `date` (with timezone), `draft:false`, `tags`, `aliases` (old path), and `categories` (ignores the WordPress catch‑all “Allgemein”).
- Converts post content to **Markdown**, keeping **text ↔ image order**; inline emoji images are replaced by real Unicode emojis.
- Downloads **original** images (strips WordPress `-WxH` / `-scaled` suffixes) and links them **locally**:
  - Galleries → `static/galleries/$slug/...`
  - Single images → `static/images/$slug/...`
- Cleans output folders on start (by default): `content/posts`, `static/images`, `static/galleries` (`-clean=false` to keep).
- Parallel downloads with simple retry/backoff on timeouts.

## Quickstart

```bash
git clone <your-repo-url> wordpress2hugo
cd wordpress2hugo

go mod init wordpress2hugo
go get github.com/PuerkitoBio/goquery github.com/JohannesKaufmann/html-to-markdown github.com/mmcdole/gofeed gopkg.in/yaml.v3

# Example feed (not your blog), process one item
go run . -feed https://wordpress.org/news/feed/   -out content/posts -static static -tz Europe/Berlin -concurrency 8 -limit 1
```

> Tip: Use `go run .` so it works regardless of the filename.

## Flags

- `-feed` (string): Feed URL or file path (e.g., `https://example.com/feed/`).
- `-out` (string): Output directory for Markdown (default `content/posts`).
- `-static` (string): Hugo `static` root (default `static`). Images go into `static/images` and `static/galleries`.
- `-tz` (string): IANA timezone for dates (default `Europe/Berlin`).
- `-limit` (int): Number of items to process (default **1**; `0` = all).
- `-concurrency` (int): Concurrent image download workers.
- `-clean` (bool): Delete output folders before run (default **true**).
- `-v` (bool): Verbose logs (default **true**).

## Output layout

```
content/
  posts/
    2023-11-my-title.md
static/
  images/
    2023-11-my-title/*.jpg
  galleries/
    2023-11-my-title/*.jpg
```

## Notes

- Emojis in the text are preserved; emoji images from `s.w.org` are replaced with their Unicode character.
- If an original image fails to download, the tool retries up to 3 times with a small backoff.
- Tested with Go ≥ 1.20.
