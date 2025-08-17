package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	markdown "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
	"gopkg.in/yaml.v3"
)

// RSS structs with namespace support
// Note: encoding/xml matches namespaced elements when using the expanded form {namespace}local

type RSS struct {
	Channel Channel `xml:"channel"`
}

type Channel struct {
	Title string `xml:"title"`
	Items []Item `xml:"item"`
}

type Item struct {
	Title           string     `xml:"title"`
	Link            string     `xml:"link"`
	PubDate         string     `xml:"pubDate"`
	GUID            string     `xml:"guid"`
	Creator         string     `xml:"{http://purl.org/dc/elements/1.1/}creator"`
	Description     string     `xml:"description"`
	ContentEncoded  string     `xml:"{http://purl.org/rss/1.0/modules/content/}encoded"`
	Categories      []Category `xml:"category"`
	CommentsFeedURL string     `xml:"{http://wellformedweb.org/CommentAPI/}commentRss"`
}

type Category struct {
	Domain string `xml:"domain,attr"`
	Value  string `xml:",chardata"`
}

// Front matter structure for YAML

type FrontMatter struct {
	Title      string    `yaml:"title"`
	Date       time.Time `yaml:"date"`
	Draft      bool      `yaml:"draft"`
	Tags       []string  `yaml:"tags"`
	Aliases    []string  `yaml:"aliases"`
	Categories []string  `yaml:"categories"`
}

var (
	feedURL     = flag.String("feed", "https://blog.breyer.berlin/feed/", "RSS feed URL or file path")
	outDir      = flag.String("out", "content/posts", "Output directory for Hugo Markdown files")
	staticDir   = flag.String("static", "static", "Hugo static directory (root of images/galleries)")
	timezone    = flag.String("tz", "Europe/Berlin", "IANA timezone for front matter dates, e.g. Europe/Berlin")
	limitItems  = flag.Int("limit", 0, "Process only the first N items (0 = all)")
	concurrency = flag.Int("concurrency", 6, "Concurrent image download workers")
	verbose     = flag.Bool("v", true, "Verbose output")
)

func main() {
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}
	if err := os.MkdirAll(*staticDir, 0o755); err != nil {
		log.Fatalf("create static dir: %v", err)
	}

	rss, err := loadRSS(*feedURL)
	if err != nil {
		log.Fatalf("load RSS: %v", err)
	}

	loc, err := time.LoadLocation(*timezone)
	if err != nil {
		log.Printf("warn: could not load tz %q, using Local: %v", *timezone, err)
		loc = time.Local
	}

	// Image downloader with deduplication
	dl := newDownloader(*concurrency)

	n := len(rss.Channel.Items)
	if *limitItems > 0 && *limitItems < n {
		n = *limitItems
	}

	for i := 0; i < n; i++ {
		item := rss.Channel.Items[i]
		if err := processItem(item, loc, dl); err != nil {
			log.Printf("error processing item %d: %v", i, err)
		}
	}

	dl.Wait()
}

func loadRSS(src string) (*RSS, error) {
	var r io.ReadCloser
	var err error

	src = strings.TrimSpace(src)
	src = strings.TrimPrefix(src, "view-source:") // allow pasted view-source: URLs

	if fileExists(src) {
		r, err = os.Open(src)
		if err != nil {
			return nil, err
		}
	} else {
		client := &http.Client{Timeout: 30 * time.Second}
		req, err := http.NewRequest("GET", src, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		r = resp.Body
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Try robust feed parsing with gofeed (handles many malformed feeds)
	fp := gofeed.NewParser()
	feed, err := fp.ParseString(string(data))
	if err != nil {
		// As a fallback, try sanitizing obvious issues and reparse
		safe := sanitizeXML(data)
		feed, err = fp.ParseString(string(safe))
		if err != nil {
			return nil, fmt.Errorf("failed to parse feed: %w", err)
		}
	}

	out := &RSS{Channel: Channel{Title: feed.Title}}
	for _, it := range feed.Items {
		pub := it.Published
		if pub == "" && it.PublishedParsed != nil {
			pub = it.PublishedParsed.Format(time.RFC1123Z)
		}
		creator := ""
		if it.Author != nil {
			creator = strings.TrimSpace(it.Author.Name)
		}
		// Prefer full HTML content; fall back to description
		html := it.Content
		if strings.TrimSpace(html) == "" {
			html = it.Description
		}

		// Categories: gofeed gives plain strings (domain attr from WP isn't preserved)
		cats := make([]Category, 0, len(it.Categories))
		for _, c := range it.Categories {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			cats = append(cats, Category{Value: c})
		}

		// Comments feed (best-effort via extensions)
		commentsURL := ""
		if extNS, ok := it.Extensions["wfw"]; ok {
			if nodes, ok := extNS["commentRss"]; ok && len(nodes) > 0 {
				commentsURL = strings.TrimSpace(nodes[0].Value)
			}
		}

		out.Channel.Items = append(out.Channel.Items, Item{
			Title:           it.Title,
			Link:            it.Link,
			PubDate:         pub,
			GUID:            it.GUID,
			Creator:         creator,
			Description:     it.Description,
			ContentEncoded:  html,
			Categories:      cats,
			CommentsFeedURL: commentsURL,
		})
	}
	return out, nil
}

// charsetReader lets encoding/xml handle non-UTF8 feeds if needed
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	// Most WP feeds are UTF-8, so just return input
	return input, nil
}

func htmlEntityMap() map[string]string {
	return map[string]string{
		"nbsp":   " ",
		"raquo":  "»",
		"laquo":  "«",
		"ndash":  "–",
		"mdash":  "—",
		"hellip": "…",
		"copy":   "©",
		"reg":    "®",
		"trade":  "™",
		"rsquo":  "’",
		"lsquo":  "‘",
		"rdquo":  "”",
		"ldquo":  "“",
		"euro":   "€",
		"amp":    "&",
		"lt":     "<",
		"gt":     ">",
		"quot":   "\"",
		"apos":   "'",
	}
}

func sanitizeXML(b []byte) []byte {
	s := string(b)
	s = removeInvalidXMLChars(s)
	s = encodeAmpersands(s)
	return []byte(s)
}

func encodeAmpersands(s string) string {
	var out strings.Builder
	out.Grow(len(s) + len(s)/100)
	for i := 0; i < len(s); {
		c := s[i]
		if c != '&' {
			out.WriteByte(c)
			i++
			continue
		}
		j := i + 1
		if j < len(s) {
			if s[j] == '#' {
				j++
				if j < len(s) && (s[j] == 'x' || s[j] == 'X') {
					j++
					k := j
					for k < len(s) && isHex(s[k]) {
						k++
					}
					if k < len(s) && k > j && s[k] == ';' {
						out.WriteString(s[i : k+1])
						i = k + 1
						continue
					}
				} else {
					k := j
					for k < len(s) && isDigit(s[k]) {
						k++
					}
					if k < len(s) && k > j && s[k] == ';' {
						out.WriteString(s[i : k+1])
						i = k + 1
						continue
					}
				}
			} else if isAlpha(s[j]) {
				k := j + 1
				for k < len(s) && isAlphaNum(s[k]) {
					k++
				}
				if k < len(s) && k > j && s[k] == ';' {
					out.WriteString(s[i : k+1])
					i = k + 1
					continue
				}
			}
		}
		// Not a valid entity → escape
		out.WriteString("&amp;")
		i++
	}
	return out.String()
}

func isAlpha(b byte) bool    { return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') }
func isDigit(b byte) bool    { return b >= '0' && b <= '9' }
func isAlphaNum(b byte) bool { return isAlpha(b) || isDigit(b) }
func isHex(b byte) bool      { return isDigit(b) || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f') }

func removeInvalidXMLChars(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if r == 0x9 || r == 0xA || r == 0xD ||
			(r >= 0x20 && r <= 0xD7FF) ||
			(r >= 0xE000 && r <= 0xFFFD) ||
			(r >= 0x10000 && r <= 0x10FFFF) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func processItem(item Item, loc *time.Location, dl *downloader) error {
	u, err := url.Parse(strings.TrimSpace(item.Link))
	if err != nil {
		return fmt.Errorf("parse link: %w", err)
	}
	aliasPath := ensureTrailingSlash(u.Path)
	year, month, slugTail := extractPathParts(u.Path)
	if year == "" || month == "" || slugTail == "" {
		// fallback to date + normalized title
		if *verbose {
			log.Printf("fallback slug logic for link=%s", item.Link)
		}
		year, month = pubDateYearMonth(item.PubDate, loc)
		slugTail = slugify(path.Base(strings.Trim(u.Path, "/")))
	}
	slug := fmt.Sprintf("%s-%s-%s", year, month, slugTail)

	contentHTML := strings.TrimSpace(item.ContentEncoded)
	if contentHTML == "" {
		contentHTML = strings.TrimSpace(item.Description)
	}

	processedHTML, err := rewriteAndDownloadImages(contentHTML, slug, dl)
	if err != nil {
		return fmt.Errorf("rewrite images: %w", err)
	}

	md, err := htmlToMarkdown(processedHTML, u)
	if err != nil {
		return fmt.Errorf("html->md: %w", err)
	}

	postTime, err := parsePubDate(item.PubDate, loc)
	if err != nil {
		if *verbose {
			log.Printf("warn: pubDate parse failed, using now: %v", err)
		}
		postTime = time.Now().In(loc)
	}

	tags, cats := splitTagsAndCategories(item.Categories)
	aliases := []string{aliasPath}

	fm := FrontMatter{
		Title:      strings.TrimSpace(item.Title),
		Date:       postTime,
		Draft:      false,
		Tags:       tags,
		Aliases:    aliases,
		Categories: cats,
	}

	if err := writeMarkdownFile(slug, fm, md); err != nil {
		return err
	}

	if *verbose {
		log.Printf("✓ %s -> %s.md (%d chars)", item.Title, slug, len(md))
	}
	return nil
}

func writeMarkdownFile(slug string, fm FrontMatter, body string) error {
	data, err := yaml.Marshal(&fm)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(data)
	buf.WriteString("---\n")
	buf.WriteString(strings.TrimSpace(body))
	buf.WriteString("\n")

	outPath := filepath.Join(*outDir, slug+".md")
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, buf.Bytes(), 0o644)
}

func splitTagsAndCategories(cats []Category) (tags []string, categories []string) {
	mTags := map[string]struct{}{}
	mCats := map[string]struct{}{}
	for _, c := range cats {
		name := strings.TrimSpace(htmlUnescape(c.Value))
		if name == "" {
			continue
		}
		if strings.EqualFold(c.Domain, "post_tag") {
			mTags[name] = struct{}{}
		} else {
			mCats[name] = struct{}{}
		}
	}
	tags = setToSortedSlice(mTags)
	categories = setToSortedSlice(mCats)
	return
}

func setToSortedSlice(m map[string]struct{}) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

func htmlToMarkdown(html string, base *url.URL) (string, error) {
	conv := markdown.NewConverter(base.String(), true, nil)
	// Tweak rules if desired
	return conv.ConvertString(html)
}

func parsePubDate(p string, loc *time.Location) (time.Time, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return time.Time{}, errors.New("empty pubDate")
	}
	// Try common RSS formats
	formats := []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822, time.RFC3339}
	var t time.Time
	var err error
	for _, f := range formats {
		t, err = time.Parse(f, p)
		if err == nil {
			return t.In(loc), nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown date format: %q", p)
}

func pubDateYearMonth(p string, loc *time.Location) (string, string) {
	t, err := parsePubDate(p, loc)
	if err != nil {
		now := time.Now().In(loc)
		return fmt.Sprintf("%04d", now.Year()), fmt.Sprintf("%02d", int(now.Month()))
	}
	return fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month()))
}

func extractPathParts(p string) (year, month, tail string) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if len(segs) >= 4 {
		year = segs[0]
		month = segs[1]
		tail = segs[3]
		return
	}
	return "", "", ""
}

func ensureTrailingSlash(p string) string {
	if p == "" {
		return "/"
	}
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

var slugRe = regexp.MustCompile(`[^a-z0-9\-]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func htmlUnescape(s string) string {
	// Minimal replacement; XML decoder already unescapes most values
	return strings.ReplaceAll(s, "\u00a0", " ")
}

func rewriteAndDownloadImages(html string, slug string, dl *downloader) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	doc.Find("img").Each(func(i int, s *goquery.Selection) {
		parentGallery := s.ParentsFiltered(".wp-block-gallery")
		isGallery := parentGallery.Length() > 0
		src, _ := s.Attr("src")
		srcset, _ := s.Attr("srcset")
		best := pickBestSrc(src, srcset)
		if best == "" {
			return
		}
		base := filepath.Join(*staticDir, "images", slug)
		relBase := filepath.ToSlash(path.Join("/images", slug))
		if isGallery {
			base = filepath.Join(*staticDir, "galleries", slug)
			relBase = filepath.ToSlash(path.Join("/galleries", slug))
		}
		_ = os.MkdirAll(base, 0o755)

		filename := filenameFromURL(best)
		dest := filepath.Join(base, filename)
		rel := path.Join(relBase, filename)

		dl.Schedule(best, dest)

		s.RemoveAttr("srcset")
		s.RemoveAttr("sizes")
		s.SetAttr("src", rel)
	})

	// Serialize modified HTML back to string (inner contents)
	var outParts []string
	root := doc.Selection
	// Prefer body contents if a body exists
	if doc.Find("body").Length() > 0 {
		doc.Find("body").Contents().Each(func(i int, s *goquery.Selection) {
			h, err := goquery.OuterHtml(s)
			if err == nil {
				outParts = append(outParts, h)
			}
		})
	} else {
		root.Contents().Each(func(i int, s *goquery.Selection) {
			h, err := goquery.OuterHtml(s)
			if err == nil {
				outParts = append(outParts, h)
			}
		})
	}
	return strings.TrimSpace(strings.Join(outParts, "")), nil
}

var srcsetRe = regexp.MustCompile(`,?\s*([^\s,]+)\s+(\d+)w`)

func pickBestSrc(src string, srcset string) string {
	src = strings.TrimSpace(src)
	srcset = strings.TrimSpace(srcset)
	best := src
	maxW := -1
	for _, m := range srcsetRe.FindAllStringSubmatch(srcset, -1) {
		u := m[1]
		wStr := m[2]
		var w int
		fmt.Sscanf(wStr, "%d", &w)
		if w > maxW {
			maxW = w
			best = u
		}
	}
	return best
}

func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return path.Base(raw)
	}
	name := path.Base(u.Path)
	if name == "" || name == "/" {
		name = "image"
	}
	return name
}

// Downloader implements deduplicated concurrent downloads

type downloader struct {
	wg   sync.WaitGroup
	sem  chan struct{}
	seen sync.Map // url -> struct{}
}

func newDownloader(concurrency int) *downloader {
	if concurrency < 1 {
		concurrency = 1
	}
	return &downloader{sem: make(chan struct{}, concurrency)}
}

func (d *downloader) Schedule(url string, dest string) {
	if _, exists := d.seen.LoadOrStore(url, struct{}{}); exists {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.sem <- struct{}{}
		defer func() { <-d.sem }()
		if err := downloadFile(url, dest); err != nil {
			log.Printf("download failed %s -> %s: %v", url, dest, err)
		} else if *verbose {
			log.Printf("downloaded %s", dest)
		}
	}()
}

func (d *downloader) Wait() { d.wg.Wait() }

func downloadFile(rawURL, dest string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "rss2hugo/1.0 (+https://example.com)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
