package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultUserAgent = "hydra-build-status/2.0 (+https://github.com/imincik/nix-utils; friendly Nix CI bot)"
	defaultHydraURL  = "https://hydra.nixos.org"
	defaultCacheURL  = "https://cache.nixos.org"
)

var (
	// statusImgRe extracts the alt text from the Hydra build status image.
	statusImgRe = regexp.MustCompile(`<img[^>]+class=["']build-status["'][^>]*alt=["']([^"']+)["']|<img[^>]+alt=["']([^"']+)["'][^>]*class=["']build-status["']`)
	// timestampRe extracts the build timestamp from Hydra's <time> tag.
	timestampRe = regexp.MustCompile(`<time[^>]+data-timestamp=["']([0-9]+)["']`)

	// okStatuses are build statuses that are not considered failures.
	okStatuses = map[string]bool{
		"Succeeded":          true,
		"Succeeded (cached)": true,
		"Cancelled":          true,
		"No data":            true,
		"No recent data":     true,
	}
)

type PackageItem struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	System string `json:"system,omitempty"`
}

type CheckResult struct {
	Name   string
	Status string
	URL    string
	IsOK   bool
}

// Config holds runtime CLI options.
type Config struct {
	File          string
	Platforms     []string
	FailedOnly    bool
	Project       string
	Jobset        string
	CacheURL      string
	NoCache       bool
	CacheOnly     bool
	Workers       int
	HydraInterval time.Duration
	Verbose       bool
}

func parseFlags() Config {
	var (
		fileFlag, fileShort           string
		platformsFlag, platformsShort string
		failedOnly, failedOnlyShort   bool
		projectFlag                   string
		jobsetFlag                    string
		cacheURLFlag                  string
		noCacheFlag                   bool
		cacheOnlyFlag                 bool
		workersFlag                   int
		hydraIntervalFlag             time.Duration
		verboseFlag                   bool
	)

	flag.StringVar(&fileFlag, "file", "", "Path to packages JSON file")
	flag.StringVar(&fileShort, "f", "", "Path to packages JSON file (shorthand)")

	flag.StringVar(&platformsFlag, "platforms", "x86_64-linux,aarch64-linux,x86_64-darwin,aarch64-darwin", "Comma-separated list of platforms")
	flag.StringVar(&platformsShort, "p", "", "Comma-separated list of platforms (shorthand)")

	flag.BoolVar(&failedOnly, "failed-only", false, "Only print failing packages")
	flag.BoolVar(&failedOnlyShort, "x", false, "Only print failing packages (shorthand)")

	flag.StringVar(&projectFlag, "project", "nixpkgs", "Hydra project name")
	flag.StringVar(&jobsetFlag, "jobset", "unstable", "Hydra jobset name")
	flag.StringVar(&cacheURLFlag, "cache-url", defaultCacheURL, "Nix binary cache URL")
	flag.BoolVar(&noCacheFlag, "no-cache", false, "Disable binary cache pre-check and query Hydra directly")
	flag.BoolVar(&cacheOnlyFlag, "cache-only", false, "Only check binary cache, never query Hydra")
	flag.IntVar(&workersFlag, "workers", 16, "Concurrency for binary cache HEAD requests")
	flag.DurationVar(&hydraIntervalFlag, "hydra-interval", 1000*time.Millisecond, "Minimum interval between consecutive Hydra requests")
	flag.BoolVar(&verboseFlag, "verbose", false, "Enable verbose log output")

	flag.Parse()

	f := fileFlag
	if f == "" {
		f = fileShort
	}
	if f == "" {
		fmt.Fprintln(os.Stderr, "Error: --file (-f) argument is required.")
		flag.Usage()
		os.Exit(2)
	}

	p := platformsFlag
	if platformsShort != "" {
		p = platformsShort
	}
	var platforms []string
	for _, plat := range strings.Split(p, ",") {
		plat = strings.TrimSpace(plat)
		if plat != "" {
			platforms = append(platforms, plat)
		}
	}

	fo := failedOnly || failedOnlyShort

	return Config{
		File:          f,
		Platforms:     platforms,
		FailedOnly:    fo,
		Project:       projectFlag,
		Jobset:        jobsetFlag,
		CacheURL:      strings.TrimRight(cacheURLFlag, "/"),
		NoCache:       noCacheFlag,
		CacheOnly:     cacheOnlyFlag,
		Workers:       workersFlag,
		HydraInterval: hydraIntervalFlag,
		Verbose:       verboseFlag,
	}
}

// loadPackages normalizes various JSON shapes into []PackageItem.
func loadPackages(filePath string) ([]PackageItem, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading packages file: %w", err)
	}

	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON in packages file: %w", err)
	}

	// Handle top-level object {"packages": [...]}
	if m, ok := raw.(map[string]any); ok {
		if pkgsList, hasPkgs := m["packages"]; hasPkgs {
			raw = pkgsList
		}
	}

	var items []PackageItem

	switch v := raw.(type) {
	case []any:
		for _, elem := range v {
			switch e := elem.(type) {
			case string:
				items = append(items, PackageItem{Name: e})
			case map[string]any:
				var item PackageItem
				if n, ok := e["name"].(string); ok {
					item.Name = n
				} else if n, ok := e["pname"].(string); ok {
					item.Name = n
				} else if n, ok := e["attr"].(string); ok {
					item.Name = n
				}

				if p, ok := e["path"].(string); ok {
					item.Path = p
				} else if p, ok := e["storePath"].(string); ok {
					item.Path = p
				} else if p, ok := e["out"].(string); ok {
					item.Path = p
				} else if p, ok := e["outPath"].(string); ok {
					item.Path = p
				}

				if s, ok := e["system"].(string); ok {
					item.System = s
				} else if s, ok := e["platform"].(string); ok {
					item.System = s
				}

				if item.Name != "" {
					items = append(items, item)
				}
			}
		}
	case map[string]any:
		for k, val := range v {
			switch valTyped := val.(type) {
			case string:
				items = append(items, PackageItem{Name: k, Path: valTyped})
			case map[string]any:
				item := PackageItem{Name: k}
				if p, ok := valTyped["path"].(string); ok {
					item.Path = p
				} else if p, ok := valTyped["storePath"].(string); ok {
					item.Path = p
				} else if p, ok := valTyped["out"].(string); ok {
					item.Path = p
				} else if p, ok := valTyped["outPath"].(string); ok {
					item.Path = p
				}
				if s, ok := valTyped["system"].(string); ok {
					item.System = s
				} else if s, ok := valTyped["platform"].(string); ok {
					item.System = s
				}
				items = append(items, item)
			default:
				items = append(items, PackageItem{Name: k})
			}
		}
	default:
		return nil, fmt.Errorf("unexpected JSON format (expected list or object)")
	}

	return items, nil
}

// extractStoreHash returns the 32-character base32 hash from a store path.
func extractStoreHash(storePath string) string {
	if storePath == "" {
		return ""
	}
	base := filepath.Base(storePath)
	parts := strings.SplitN(base, "-", 2)
	if len(parts) > 0 && len(parts[0]) == 32 {
		return parts[0]
	}
	return ""
}

// cacheChecker queries cache.nixos.org with HTTP HEAD requests.
type cacheChecker struct {
	client   *http.Client
	cacheURL string
	workers  int
}

func newCacheChecker(cacheURL string, workers int) *cacheChecker {
	transport := &http.Transport{
		MaxIdleConns:        workers * 2,
		MaxIdleConnsPerHost: workers,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	return &cacheChecker{
		client:   client,
		cacheURL: cacheURL,
		workers:  workers,
	}
}

// checkAll checks binary cache for all eligible packages concurrently.
// Returns a map of packageName -> (isCached, narinfoURL).
func (c *cacheChecker) checkAll(ctx context.Context, pkgs []PackageItem, platform string) map[string]CheckResult {
	results := make(map[string]CheckResult)
	var mu sync.Mutex

	type workItem struct {
		pkg  PackageItem
		hash string
	}

	var eligible []workItem
	for _, p := range pkgs {
		if p.Path == "" {
			continue
		}
		// If item specified a platform, ensure it matches current platform.
		if p.System != "" && p.System != platform {
			continue
		}
		h := extractStoreHash(p.Path)
		if h != "" {
			eligible = append(eligible, workItem{pkg: p, hash: h})
		}
	}

	if len(eligible) == 0 {
		return results
	}

	sem := make(chan struct{}, c.workers)
	var wg sync.WaitGroup

	for _, item := range eligible {
		wg.Add(1)
		go func(it workItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			url := fmt.Sprintf("%s/%s.narinfo", c.cacheURL, it.hash)
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", defaultUserAgent)

			// Simple retry with jitter for transient network glitches
			var resp *http.Response
			for attempt := 0; attempt < 2; attempt++ {
				resp, err = c.client.Do(req)
				if err == nil {
					break
				}
				time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)
			}

			if err == nil && resp != nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					mu.Lock()
					results[it.pkg.Name] = CheckResult{
						Name:   it.pkg.Name,
						Status: "Succeeded",
						URL:    url,
						IsOK:   true,
					}
					mu.Unlock()
				}
			}
		}(item)
	}

	wg.Wait()
	return results
}

// hydraClient queries Hydra while strictly enforcing polite rate-limiting,
// exponential backoff on 429/503, and a circuit breaker to prevent DoS.
type hydraClient struct {
	client               *http.Client
	minInterval          time.Duration
	lastRequest          time.Time
	consecutiveFailures  int
	circuitBreakerOpen   bool
	circuitBreakerReason string
	mu                   sync.Mutex
}

func newHydraClient(minInterval time.Duration) *hydraClient {
	transport := &http.Transport{
		MaxIdleConns:        5,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
	}
	return &hydraClient{
		client:      client,
		minInterval: minInterval,
	}
}

// queryStatus fetches the latest build status for a job from Hydra.
func (h *hydraClient) queryStatus(ctx context.Context, project, jobset, pkgName, platform string) (status, jobURL string) {
	jobURL = fmt.Sprintf("%s/job/%s/%s/%s.%s/all", defaultHydraURL, project, jobset, pkgName, platform)

	h.mu.Lock()
	defer h.mu.Unlock()

	// If circuit breaker is tripped, immediately return without hitting Hydra
	if h.circuitBreakerOpen {
		return fmt.Sprintf("Circuit Breaker (%s)", h.circuitBreakerReason), jobURL
	}

	// Rate limiter: ensure at least minInterval has elapsed since last request
	elapsed := time.Since(h.lastRequest)
	if elapsed < h.minInterval {
		time.Sleep(h.minInterval - elapsed)
	}
	h.lastRequest = time.Now()

	// Adaptive retry with exponential backoff on 429 / 503
	var body []byte
	var statusCode int

	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, jobURL, nil)
		if err != nil {
			return "Request error", jobURL
		}
		req.Header.Set("User-Agent", defaultUserAgent)

		resp, err := h.client.Do(req)
		if err != nil {
			h.consecutiveFailures++
			if h.consecutiveFailures >= 3 {
				h.circuitBreakerOpen = true
				h.circuitBreakerReason = "Hydra network timeout"
				fmt.Fprintln(os.Stderr, "WARNING: Consecutive Hydra timeouts. Tripping circuit breaker to avoid DoS.")
				return "Hydra Timeout (Throttled)", jobURL
			}
			time.Sleep(time.Duration(1000*(1<<attempt)+rand.Intn(500)) * time.Millisecond)
			continue
		}

		statusCode = resp.StatusCode
		b, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable {
			h.consecutiveFailures++
			if h.consecutiveFailures >= 3 {
				h.circuitBreakerOpen = true
				h.circuitBreakerReason = fmt.Sprintf("HTTP %d", statusCode)
				fmt.Fprintf(os.Stderr, "WARNING: Hydra returned HTTP %d repeatedly. Tripping circuit breaker to prevent DoS.\n", statusCode)
				return fmt.Sprintf("Throttled (HTTP %d)", statusCode), jobURL
			}

			// Respect Retry-After if provided, else exponential backoff with jitter
			retryWait := time.Duration(2*(1<<attempt))*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if s, err := strconv.Atoi(ra); err == nil && s > 0 && s < 60 {
					retryWait = time.Duration(s) * time.Second
				}
			}
			time.Sleep(retryWait)
			continue
		}

		if readErr != nil {
			return "Read error", jobURL
		}

		// Reset consecutive failures on success
		h.consecutiveFailures = 0
		body = b
		break
	}

	if statusCode != http.StatusOK {
		if statusCode == 0 {
			return "Connection error", jobURL
		}
		return fmt.Sprintf("HTTP %d", statusCode), jobURL
	}

	html := string(body)
	match := statusImgRe.FindStringSubmatch(html)
	if len(match) == 0 {
		return "No data", jobURL
	}

	st := match[1]
	if st == "" && len(match) > 2 {
		st = match[2]
	}

	tsMatch := timestampRe.FindStringSubmatch(html)
	if len(tsMatch) > 1 {
		ts, err := strconv.ParseInt(tsMatch[1], 10, 64)
		if err == nil {
			buildTime := time.Unix(ts, 0)
			if time.Since(buildTime) > 365*24*time.Hour {
				return "No recent data", jobURL
			}
		}
	}

	return st, jobURL
}

func printHeader(platform string) {
	fmt.Printf("\n### PLATFORM: %s\n\n", platform)
	fmt.Printf("| %-50s | %-20s | %-80s |\n", "PACKAGE", "STATUS", "URL")
	fmt.Printf("| %-50s | %-20s | %-80s |\n", strings.Repeat("-", 20), strings.Repeat("-", 20), strings.Repeat("-", 20))
}

func main() {
	cfg := parseFlags()

	pkgs, err := loadPackages(cfg.File)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	if cfg.Verbose {
		fmt.Fprintf(os.Stderr, "Loaded %d packages from %s\n", len(pkgs), cfg.File)
	}

	ctx := context.Background()
	cache := newCacheChecker(cfg.CacheURL, cfg.Workers)
	hydra := newHydraClient(cfg.HydraInterval)

	exitCode := 0

	for _, platform := range cfg.Platforms {
		headerPrinted := false

		if !cfg.FailedOnly {
			printHeader(platform)
			headerPrinted = true
		}

		// Phase 1: Fast CDN Binary Cache Pre-Check
		var cachedResults map[string]CheckResult
		if !cfg.NoCache {
			cachedResults = cache.checkAll(ctx, pkgs, platform)
			if cfg.Verbose {
				fmt.Fprintf(os.Stderr, "[%s] Found %d packages cached on %s\n", platform, len(cachedResults), cfg.CacheURL)
			}
		} else {
			cachedResults = make(map[string]CheckResult)
		}

		// Phase 2: Process all packages in stable order, falling back to Hydra for cache misses
		for _, pkg := range pkgs {
			var status, finalURL string

			if res, ok := cachedResults[pkg.Name]; ok {
				status = res.Status
				finalURL = res.URL
			} else if cfg.CacheOnly {
				status = "Not cached"
				finalURL = fmt.Sprintf("%s/job/%s/%s/%s.%s/all", defaultHydraURL, cfg.Project, cfg.Jobset, pkg.Name, platform)
			} else {
				// Cache miss or no store path: query Hydra with polite rate-limiting & backoff
				status, finalURL = hydra.queryStatus(ctx, cfg.Project, cfg.Jobset, pkg.Name, platform)
			}

			isOK := okStatuses[status]
			if !isOK {
				exitCode = 1
			}

			if !cfg.FailedOnly || !isOK {
				if !headerPrinted {
					printHeader(platform)
					headerPrinted = true
				}
				fmt.Printf("| %-50s | %-20s | %-80s |\n", pkg.Name, status, finalURL)
			}
		}
	}

	os.Exit(exitCode)
}
