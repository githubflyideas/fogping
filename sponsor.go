package main

// Sponsor slot cache.
//
// The sponsor box in the web UI shows up to four image slides listed in
// https://github.com/githubflyideas/ads-sponsor. The program fetches that
// manifest and its images itself, on startup and then about hourly, keeps the
// last good copy under <data>/sponsor/ so it survives restarts and offline
// periods, and serves it from /sponsor/sponsors.json and /sponsor/img/<n>.
//
// It is deliberately unimportant: it runs in its own goroutine, uses its own
// short-timeout HTTP client, never touches the store or the probes, and a
// failure only means the page falls back to fetching from GitHub itself, and
// then to the built-in sponsor text.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sponsorBaseURL     = "https://raw.githubusercontent.com/githubflyideas/ads-sponsor/main/"
	sponsorMaxSlides   = 4
	sponsorMaxManifest = 64 << 10
	sponsorMaxImage    = 2 << 20
	sponsorHTTPTimeout = 10 * time.Second
)

// sponsorSlide is one entry of the manifest, both as fetched and as served.
type sponsorSlide struct {
	Image string `json:"image"`
	Link  string `json:"link,omitempty"`
	Alt   string `json:"alt"`
	Title string `json:"title,omitempty"`
}

type sponsorManifest struct {
	Version json.RawMessage `json:"version,omitempty"`
	Slides  []sponsorSlide  `json:"slides"`
}

type sponsorImage struct {
	data []byte
	ct   string
	tag  string // short content hash, used as cache-buster and ETag
}

type sponsorCache struct {
	product string
	src     string // manifest URL
	dir     string // <data>/sponsor
	client  *http.Client
	ua      string

	mu     sync.RWMutex
	slides []sponsorSlide // Image holds the original URL; served rewritten
	imgs   []sponsorImage

	failing bool // last refresh failed; used to log state changes only
}

// sponsor is set by main before the web server starts. A nil cache (tests,
// or a data directory we could not use) serves 404 and the page falls back.
var sponsor *sponsorCache

func newSponsorCache(product, dataDir string) *sponsorCache {
	c := &sponsorCache{
		product: product,
		src:     sponsorBaseURL + product + "/sponsors.json",
		dir:     filepath.Join(dataDir, "sponsor"),
		client:  &http.Client{Timeout: sponsorHTTPTimeout},
		ua:      product + "/" + version,
	}
	if err := c.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("sponsor: ignoring cached copy: %v", err)
	}
	return c
}

// run refreshes on startup and then about hourly, until stop is closed.
func (c *sponsorCache) run(stop <-chan struct{}) {
	wait := time.Duration(2+rand.Intn(10)) * time.Second
	for {
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		err := c.refresh(ctx)
		cancel()
		if err != nil {
			if !c.failing {
				log.Printf("sponsor: refresh failed (keeping last good copy, will retry): %v", err)
			}
			c.failing = true
			wait = 10*time.Minute + time.Duration(rand.Intn(300))*time.Second
			continue
		}
		if c.failing {
			log.Printf("sponsor: refresh ok again")
		}
		c.failing = false
		wait = 50*time.Minute + time.Duration(rand.Intn(1200))*time.Second
	}
}

func (c *sponsorCache) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", u, limit)
	}
	return b, nil
}

// refresh fetches the manifest and images and, if at least one slide is
// usable, replaces the cached copy on disk and in memory.
func (c *sponsorCache) refresh(ctx context.Context) error {
	raw, err := c.get(ctx, c.src, sponsorMaxManifest)
	if err != nil {
		return err
	}
	slides, err := parseSponsorManifest(raw, c.src)
	if err != nil {
		return err
	}
	var keep []sponsorSlide
	var imgs []sponsorImage
	for _, s := range slides {
		b, err := c.get(ctx, s.Image, sponsorMaxImage)
		if err != nil {
			continue
		}
		ct, err := sniffSponsorImage(b)
		if err != nil {
			continue
		}
		keep = append(keep, s)
		imgs = append(imgs, newSponsorImage(b, ct))
	}
	if len(keep) == 0 {
		return errors.New("no usable sponsor images")
	}
	if err := c.save(keep, imgs); err != nil {
		return err
	}
	c.mu.Lock()
	c.slides, c.imgs = keep, imgs
	c.mu.Unlock()
	return nil
}

func newSponsorImage(b []byte, ct string) sponsorImage {
	h := sha256.Sum256(b)
	return sponsorImage{data: b, ct: ct, tag: hex.EncodeToString(h[:6])}
}

// parseSponsorManifest validates a manifest: at most four slides, https image
// URLs (relative ones resolved against the manifest), http(s) links only, and
// bounded text. Invalid links are dropped; slides without a valid image are.
func parseSponsorManifest(raw []byte, base string) ([]sponsorSlide, error) {
	var m sponsorManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: %v", err)
	}
	bu, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	var out []sponsorSlide
	for _, s := range m.Slides {
		if len(out) == sponsorMaxSlides {
			break
		}
		img := resolveSponsorURL(bu, s.Image, true)
		if img == "" {
			continue
		}
		out = append(out, sponsorSlide{
			Image: img,
			Link:  resolveSponsorURL(bu, s.Link, false),
			Alt:   sponsorClip(s.Alt, 200),
			Title: sponsorClip(s.Title, 120),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("manifest: no valid slides")
	}
	return out, nil
}

func resolveSponsorURL(base *url.URL, v string, httpsOnly bool) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 2048 {
		return ""
	}
	u, err := base.Parse(v)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme == "https" || (!httpsOnly && u.Scheme == "http") {
		return u.String()
	}
	return ""
}

func sponsorClip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// sniffSponsorImage accepts PNG, JPEG, GIF and WebP by their magic bytes.
// SVG and everything else is refused: an SVG can carry script.
func sniffSponsorImage(b []byte) (string, error) {
	switch {
	case len(b) < 16:
		return "", errors.New("not an image")
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png", nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", nil
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif", nil
	case bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp", nil
	}
	return "", errors.New("unsupported image type")
}

// ---- disk ----

type sponsorDisk struct {
	Slides []sponsorSlide `json:"slides"`
	Types  []string       `json:"types"`
}

func (c *sponsorCache) save(slides []sponsorSlide, imgs []sponsorImage) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	d := sponsorDisk{Slides: slides}
	for i, im := range imgs {
		if err := writeFileAtomic(filepath.Join(c.dir, "img-"+strconv.Itoa(i)), im.data); err != nil {
			return err
		}
		d.Types = append(d.Types, im.ct)
	}
	for i := len(imgs); i < sponsorMaxSlides; i++ {
		os.Remove(filepath.Join(c.dir, "img-"+strconv.Itoa(i)))
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	return writeFileAtomic(filepath.Join(c.dir, "manifest.json"), b)
}

func (c *sponsorCache) load() error {
	b, err := os.ReadFile(filepath.Join(c.dir, "manifest.json"))
	if err != nil {
		return err
	}
	var d sponsorDisk
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	var slides []sponsorSlide
	var imgs []sponsorImage
	for i, s := range d.Slides {
		if i == sponsorMaxSlides {
			break
		}
		data, err := os.ReadFile(filepath.Join(c.dir, "img-"+strconv.Itoa(i)))
		if err != nil {
			continue
		}
		ct, err := sniffSponsorImage(data)
		if err != nil {
			continue
		}
		slides = append(slides, s)
		imgs = append(imgs, newSponsorImage(data, ct))
	}
	c.mu.Lock()
	c.slides, c.imgs = slides, imgs
	c.mu.Unlock()
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- http ----

// registerSponsor adds the two read-only endpoints, behind the same wrapper
// that protects the page showing the slot.
func registerSponsor(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /sponsor/sponsors.json", wrap(func(w http.ResponseWriter, r *http.Request) {
		sponsor.serveManifest(w, r)
	}))
	mux.HandleFunc("GET /sponsor/img/{n}", wrap(func(w http.ResponseWriter, r *http.Request) {
		sponsor.serveImage(w, r)
	}))
}

func (c *sponsorCache) serveManifest(w http.ResponseWriter, r *http.Request) {
	out := sponsorManifest{Slides: []sponsorSlide{}}
	if c != nil {
		c.mu.RLock()
		for i, s := range c.slides {
			s.Image = "/sponsor/img/" + strconv.Itoa(i) + "?v=" + c.imgs[i].tag
			out.Slides = append(out.Slides, s)
		}
		c.mu.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(out)
}

func (c *sponsorCache) serveImage(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	var im sponsorImage
	ok := false
	if c != nil && err == nil {
		c.mu.RLock()
		if n >= 0 && n < len(c.imgs) {
			im, ok = c.imgs[n], true
		}
		c.mu.RUnlock()
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", im.ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("ETag", `"`+im.tag+`"`)
	if r.Header.Get("If-None-Match") == `"`+im.tag+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(im.data)
}
