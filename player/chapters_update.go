package player

// Chapters (upstream media.get_chapters) and the update check
// (upstream update_check.py).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mpv-shim/jfin"
)

// chapters returns the current item's chapters, fetching them once per item
// (they are not in the default item field set).
func (p *Player) chapters() []jfin.Chapter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil {
		return nil
	}
	return p.media.Video.Chapters
}

// loadChaptersLocked starts a background chapter fetch for the current item.
// The caller holds p.mu; the fetch itself never does.
func (p *Player) loadChaptersLocked() {
	if p.media == nil || p.ctx == nil {
		return
	}
	video := p.media.Video
	if len(video.Chapters) > 0 {
		return
	}
	ctx := p.ctx
	go func() {
		chs, err := video.M.C.GetItemChapters(ctx, video.ID)
		if err != nil || len(chs) == 0 {
			return
		}
		p.mu.Lock()
		if p.media != nil && p.media.Video == video {
			video.Chapters = chs
		}
		p.mu.Unlock()
	}()
}

// openChapters lists the chapters; selecting one seeks (upstream's chapter
// skip; the web UI's own chapter bar is driven by the item, not by us).
func (m *menu) openChapters() {
	chapters := m.p.chapters()
	entries := make([]menuEntry, 0, len(chapters))
	for _, ch := range chapters {
		ch := ch
		entries = append(entries, menuEntry{
			label:  fmt.Sprintf("%s  (%s)", ch.Name, clock(float64(ch.StartTicks)/1e7)),
			action: func() { m.p.Seek(float64(ch.StartTicks)/1e7, true); m.Hide() },
		})
	}
	if len(entries) == 0 {
		return
	}
	m.push("Chapters", entries, 0)
}

// --- update check ---------------------------------------------------------

// UpdateState is what the TUI/tray/menu read about a pending release.
type UpdateState struct {
	Available bool
	Version   string
	URL       string
	Checked   time.Time
}

// HasUpdate reports whether a newer release was found.
func (p *Player) HasUpdate() bool {
	p.updMu.Lock()
	defer p.updMu.Unlock()
	return p.update.Available
}

// UpdateVersion returns the newest release tag we found.
func (p *Player) UpdateVersion() string {
	p.updMu.Lock()
	defer p.updMu.Unlock()
	return p.update.Version
}

// SetVersion records our build version for the update comparison.
func (p *Player) SetVersion(v string) {
	p.mu.Lock()
	p.version = v
	p.mu.Unlock()
	currentVersion = func() string { return v }
}

// clock formats seconds as m:ss (same helper shape as the TUI's).
func clock(sec float64) string {
	if sec <= 0 {
		return "0:00"
	}
	d := int(sec)
	return fmt.Sprintf("%d:%02d", d/60, d%60)
}

// SetUpdateURL sets where to look for releases (an API endpoint returning
// {"tag_name": …, "html_url": …}). Empty disables the check.
func (p *Player) SetUpdateURL(url string) { p.updateURL = url }

// SetUpdateEnabled turns the check on/off at runtime (preference menu).
func (p *Player) SetUpdateEnabled(on bool) {
	p.updateEnabled = on
	if on {
		p.CheckUpdate()
	}
}

// CheckUpdate polls the update URL once in the background. It is a single
// request with a short timeout: never block startup, never retry in a loop.
// The URL defaults to *this project's* release feed (see main.go), not
// upstream's — otherwise a Go shim would report the Python shim's releases.
func (p *Player) CheckUpdate() {
	if p.updateURL == "" || !p.updateEnabled {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rel, err := p.fetchLatestRelease(ctx)
		if err != nil || rel == "" || !newerThanCurrent(rel) {
			return
		}
		p.updMu.Lock()
		p.update = UpdateState{Available: true, Version: rel, URL: p.updateURL, Checked: time.Now()}
		p.updMu.Unlock()
		p.log.Printf("update available: %s (%s)", rel, p.updateURL)
	}()
}

// fetchLatestRelease reads the newest release tag. It understands both shapes
// the common forges expose: GitHub/Gitea "releases/latest" (tag_name) and a
// plain "tags" list (name). A 404 simply means "no releases yet".
func (p *Player) fetchLatestRelease(ctx context.Context) (string, error) {
	get := func(url string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "mpv-shim-go")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", url, resp.Status)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return err
		}
		return json.Unmarshal(b, out)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := get(p.updateURL, &rel); err == nil && rel.TagName != "" {
		return rel.TagName, nil
	}
	// No release object: fall back to the tag list (…/tags).
	if tagsURL := strings.TrimSuffix(p.updateURL, "/latest"); strings.HasSuffix(tagsURL, "/releases") {
		var tags []struct {
			Name string `json:"name"`
		}
		if err := get(tagsURL+"/../tags", &tags); err == nil && len(tags) > 0 {
			return tags[0].Name, nil
		}
	}
	return "", fmt.Errorf("no release at %s", p.updateURL)
}

// currentVersion is set by SetVersion at startup; the package default keeps the
// comparison working in tests.
var currentVersion = func() string { return "" }

// newerThanCurrent compares "v1.2.3" style tags against our build version.
func newerThanCurrent(tag string) bool {
	cur := strings.TrimPrefix(strings.TrimSpace(currentVersion()), "v")
	newer := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if cur == "" || cur == "dev" {
		return true
	}
	return compareVersions(newer, cur) > 0
}

// compareVersions compares dotted numeric versions.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// OpenUpdatePage opens the release page in the browser (menu row, tray).
func (p *Player) OpenUpdatePage() {
	p.updMu.Lock()
	url := p.update.URL
	p.updMu.Unlock()
	if url == "" {
		return
	}
	if err := openURL(url); err != nil {
		p.log.Printf("open update page: %v", err)
	}
}

// openUpdatePage opens the release page in the browser (menu row).
func (m *menu) openUpdatePage() {
	m.Hide()
	m.p.OpenUpdatePage()
}
