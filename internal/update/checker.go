package update

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultInterval = 24 * time.Hour
	requestTimeout  = 10 * time.Second
)

var GithubLatestURL = "https://api.github.com/repos/rvben/vedetta/releases/latest"

type settingsStore interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
	DeleteSetting(key string) error
}

type Release struct {
	Version   string    `json:"version"`
	URL       string    `json:"url"`
	CheckedAt time.Time `json:"checked_at"`
}

type Status struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	URL             string `json:"url,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	Dismissed       bool   `json:"dismissed"`
	CheckedAt       string `json:"checked_at,omitempty"`
}

type Checker struct {
	current  string
	interval time.Duration
	db       settingsStore
	mu       sync.RWMutex
	latest   *Release
	cancel   context.CancelFunc
	done     chan struct{}
}

func New(current string, interval time.Duration, db settingsStore) *Checker {
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Checker{
		current:  current,
		interval: interval,
		db:       db,
	}
}

func (c *Checker) Start(ctx context.Context) {
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	go c.run(ctx)
}

func (c *Checker) Stop() {
	if c.cancel != nil {
		c.cancel()
		<-c.done
	}
}

func (c *Checker) run(ctx context.Context) {
	defer close(c.done)
	c.check()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.check()
		}
	}
}

func (c *Checker) check() {
	release, err := fetchLatestRelease()
	if err != nil {
		slog.Warn("update check failed", "error", err)
		return
	}
	c.mu.Lock()
	c.latest = release
	c.mu.Unlock()
}

func (c *Checker) CheckNow() Status {
	c.check()
	return c.Status()
}

func (c *Checker) Status() Status {
	c.mu.RLock()
	latest := c.latest
	c.mu.RUnlock()

	s := Status{Current: c.current}
	if latest == nil {
		return s
	}

	s.Latest = latest.Version
	s.URL = latest.URL
	s.CheckedAt = latest.CheckedAt.UTC().Format(time.RFC3339)
	s.UpdateAvailable = isNewer(c.current, latest.Version)

	if s.UpdateAvailable {
		dismissed, _ := c.db.GetSetting("dismissed_update_version")
		s.Dismissed = dismissed == latest.Version
	}

	return s
}

func (c *Checker) Dismiss() error {
	c.mu.RLock()
	latest := c.latest
	c.mu.RUnlock()
	if latest == nil {
		return nil
	}
	return c.db.SetSetting("dismissed_update_version", latest.Version)
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func fetchLatestRelease() (*Release, error) {
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Get(GithubLatestURL)
	if err != nil {
		return nil, fmt.Errorf("fetching latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var gr githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("decoding release: %w", err)
	}

	return &Release{
		Version:   gr.TagName,
		URL:       gr.HTMLURL,
		CheckedAt: time.Now(),
	}, nil
}

// isNewer reports whether latest is a strictly newer release than current.
//
// A current version that is not a release version is unordered against any
// release, so it never reports an update. That covers a build with no injected
// version ("dev"), a bare VCS revision, and a `git describe` string from a
// commit after a tag. Announcing an update against such a build nags forever:
// there is no release the operator can install that makes the comparison stop
// being true, so the banner stops carrying information.
func isNewer(current, latest string) bool {
	cur, ok := parseSemver(current)
	if !ok {
		return false
	}
	lat, ok := parseSemver(latest)
	if !ok {
		return false
	}
	return compareParsed(cur, lat) < 0
}

func compareSemver(a, b string) int {
	pa, _ := parseSemver(a)
	pb, _ := parseSemver(b)
	return compareParsed(pa, pb)
}

func compareParsed(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// parseSemver splits a MAJOR.MINOR.PATCH version, with an optional leading "v",
// into its numeric components. The second return value distinguishes a version
// that is genuinely 0.0.0 from a string that is not a release version at all,
// which is the distinction isNewer depends on.
func parseSemver(s string) ([3]int, bool) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var v [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}
