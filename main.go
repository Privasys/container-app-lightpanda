// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

// container-app-lightpanda is a lightweight HTTP API server that wraps the
// Lightpanda headless browser. It exposes AI-tool endpoints that can be
// discovered and invoked via the Privasys MCP (Model Context Protocol)
// standard for container apps.
//
// Each tool maps to an HTTP POST endpoint. The management service proxies
// MCP tool invocations to these endpoints through the TDX enclave.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	// The platform injects $PORT (host networking, so the listen port is the
	// host port). It is required — there is no hard-coded fallback port.
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("PORT environment variable is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/browse", handleBrowse)
	mux.HandleFunc("/configure", handleConfigure)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/healthz", handleHealth)
	// MCP-compatible endpoints so any MCP-aware agent (confidential-ai
	// orchestrator) can discover and invoke this tool over HTTP.
	mux.HandleFunc("/api/v1/mcp/tools", handleMCPTools)
	mux.HandleFunc("/api/v1/mcp/tools/", handleMCPInvoke)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("lightpanda-api listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	log.Println("shutting down…")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// ---------------------------------------------------------------------------
//  /browse — fetch a URL and return its content
// ---------------------------------------------------------------------------

type browseRequest struct {
	URL    string `json:"url"`
	Format string `json:"format"` // "markdown" or "html" (default: "markdown")

	// Wait strategy — controls when the page DOM is captured. Without one,
	// the page is dumped at "load", before content that arrives via async
	// fetch/XHR has rendered. Pages that build their content client-side
	// (search results, dashboards, SPAs) need to wait for the network to
	// settle or for a specific element to appear.
	WaitUntil    string `json:"wait_until"`    // load|domcontentloaded|networkidle|networkalmostidle|done (default: networkidle)
	WaitSelector string `json:"wait_selector"` // CSS selector to wait for before dumping
	WaitMs       int    `json:"wait_ms"`       // timeout cap for the wait condition, in ms
}

// validWaitUntil is the lifecycle-event whitelist accepted by lightpanda's
// `fetch --wait-until`. Anything else is rejected so a bad value can't reach
// the subprocess. (networkalmostidle added in lightpanda 0.3.2.)
var validWaitUntil = map[string]bool{
	"load": true, "domcontentloaded": true, "networkidle": true, "networkalmostidle": true, "done": true,
}

const (
	// 'networkidle' (capture when the network goes quiet) is the default: fast and
	// good for most pages. 'done' (wait for all page operations) is available for
	// fully-rendered capture but is slower and not always better.
	defaultWaitUntil = "networkidle"
	maxWaitMs        = 30000 // hard cap on the wait deadline (lightpanda default: 5000)
)

type browseResponse struct {
	URL     string `json:"url"`
	Format  string `json:"format"`
	Content string `json:"content"`
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req browseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required")
		return
	}

	// Validate URL scheme
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		writeError(w, http.StatusBadRequest, "url must start with http:// or https://")
		return
	}

	// Enforce the owner-configured fetch policy (see /configure).
	if !hostAllowed(req.URL) {
		writeError(w, http.StatusForbidden, "this host is not in the configured allowed-domains policy")
		return
	}

	format := req.Format
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "html" {
		writeError(w, http.StatusBadRequest, "format must be 'markdown' or 'html'")
		return
	}

	waitUntil := req.WaitUntil
	if waitUntil == "" {
		waitUntil = defaultWaitUntil
	}
	if !validWaitUntil[waitUntil] {
		writeError(w, http.StatusBadRequest, "wait_until must be one of: load, domcontentloaded, networkidle, networkalmostidle, done")
		return
	}
	waitMs := req.WaitMs
	if waitMs < 0 {
		waitMs = 0
	}
	if waitMs > maxWaitMs {
		waitMs = maxWaitMs
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	content, err := fetchURL(ctx, req.URL, format, waitUntil, req.WaitSelector, waitMs)
	if err != nil {
		log.Printf("[browse] error fetching %s: %v", req.URL, err)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("fetch failed: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, browseResponse{
		URL:     req.URL,
		Format:  format,
		Content: content,
	})
}

func fetchURL(ctx context.Context, url, format, waitUntil, waitSelector string, waitMs int) (string, error) {
	args := []string{"fetch", "--dump", format}
	if waitUntil != "" {
		args = append(args, "--wait-until", waitUntil)
	}
	if waitSelector != "" {
		args = append(args, "--wait-selector", waitSelector)
	}
	if waitMs > 0 {
		args = append(args, "--wait-ms", strconv.Itoa(waitMs))
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, "/bin/lightpanda", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// ---------------------------------------------------------------------------
//  /configure — owner-set fetch policy (configure-then-freeze, role:config)
// ---------------------------------------------------------------------------
//
// The manifest tags this endpoint role:"config", so the enclave manager keeps
// every other path at HTTP 503 ("awaiting initial configuration") until the
// first 2xx here, then auto-lifts the gate. The app holds no freeze logic and
// does not call config-complete. The policy is in-memory, so the gate re-arms
// on every restart and the owner re-submits — matching the platform contract.

var (
	policyMu       sync.RWMutex
	allowedDomains []string // lower-cased domains; subdomains allowed
	allowAllHosts  bool     // true when "*" was configured
)

type configureRequest struct {
	AllowedDomains string `json:"allowed_domains"`
}

func handleConfigure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	var req configureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var list []string
	all := false
	for _, d := range strings.Split(req.AllowedDomains, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		switch {
		case d == "":
			continue
		case d == "*":
			all = true
		default:
			list = append(list, d)
		}
	}
	if !all && len(list) == 0 {
		writeError(w, http.StatusBadRequest,
			"allowed_domains is required: a comma-separated list of domains, or * for any")
		return
	}
	policyMu.Lock()
	allowedDomains = list
	allowAllHosts = all
	policyMu.Unlock()
	// Returning 2xx lifts the manager's configure-then-freeze gate.
	writeJSON(w, http.StatusOK, map[string]string{"status": "configured"})
}

// hostAllowed reports whether the fetch policy permits the given URL's host.
func hostAllowed(rawurl string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	if allowAllHosts {
		return true
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range allowedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
//  /health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
//  /api/v1/mcp/tools[/<name>] — MCP discovery + invoke adapter
// ---------------------------------------------------------------------------

var browseToolDescriptor = map[string]any{
	"name":        "browse",
	"description": "Fetch a web page by URL and return its readable content as Markdown (or HTML). Use this to read the full content of a specific page — a URL the user gives you, or a result found via web search — when a search snippet is not enough to answer. For pages that build their content in the browser (search results, dashboards, single-page apps), use the wait_* parameters so the dynamically loaded content is captured.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The full URL of the page to read, including http:// or https://.",
			},
			"format": map[string]any{
				"type":        "string",
				"enum":        []string{"markdown", "html"},
				"description": "Return the content as 'markdown' (default, readable) or 'html' (raw).",
			},
			"wait_until": map[string]any{
				"type":        "string",
				"enum":        []string{"load", "domcontentloaded", "networkidle", "networkalmostidle", "done"},
				"description": "When to capture the page. 'networkidle' (default) captures when the network goes quiet — good for most pages. 'done' waits until ALL page operations finish (slower, fullest render). 'networkalmostidle' tolerates a couple of lingering connections (keep-alive/analytics). 'load' / 'domcontentloaded' fire earliest. This is the lever for how long to wait — wait_ms is only the deadline, not a delay.",
			},
			"wait_selector": map[string]any{
				"type":        "string",
				"description": "Optional CSS selector to wait for before capturing the page. The most precise way to wait for specific client-side-rendered content — capture happens once this element appears (or wait_ms elapses).",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Maximum time in milliseconds to wait for the wait_until / wait_selector condition before giving up and capturing whatever is there (a deadline/timeout, NOT a fixed delay — there is no fixed-sleep mode). Default 5000, max 30000. Raise it for slow pages.",
			},
		},
		"required": []string{"url"},
	},
	"requires_user_confirmation": false,
}

func handleMCPTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": []any{browseToolDescriptor},
	})
}

func handleMCPInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/mcp/tools/")
	switch name {
	case "browse":
		handleBrowse(w, r)
	default:
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown tool: %s", name))
	}
}

// ---------------------------------------------------------------------------
//  Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
