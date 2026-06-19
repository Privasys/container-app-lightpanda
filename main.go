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
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultPort = "8080"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/browse", handleBrowse)
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
	WaitUntil    string `json:"wait_until"`    // load|domcontentloaded|networkidle0|networkidle2 (default: networkidle2)
	WaitSelector string `json:"wait_selector"` // CSS selector to wait for before dumping
	WaitMs       int    `json:"wait_ms"`       // additional settle delay in ms (capped)
}

// validWaitUntil is the lifecycle-event whitelist accepted by lightpanda's
// `fetch --wait-until` (Puppeteer semantics). Anything else is rejected so a
// bad value can't reach the subprocess.
var validWaitUntil = map[string]bool{
	"load": true, "domcontentloaded": true, "networkidle0": true, "networkidle2": true,
}

const (
	defaultWaitUntil = "networkidle2" // wait until the page basically stops fetching
	maxWaitMs        = 30000          // hard cap on the caller-supplied settle delay
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
		writeError(w, http.StatusBadRequest, "wait_until must be one of: load, domcontentloaded, networkidle0, networkidle2")
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
	"description": "Fetch a web page using the Lightpanda headless browser and return its content as markdown or HTML. Call this whenever the user references a URL and you need the page contents to answer. For pages that build their content client-side (search results, dashboards, single-page apps), use the wait_* parameters so the dynamically loaded content is captured.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL of the web page to fetch (must start with http:// or https://)",
			},
			"format": map[string]any{
				"type":        "string",
				"enum":        []string{"markdown", "html"},
				"description": "Output format (default: markdown)",
			},
			"wait_until": map[string]any{
				"type":        "string",
				"enum":        []string{"load", "domcontentloaded", "networkidle0", "networkidle2"},
				"description": "When to capture the page. 'load' / 'domcontentloaded' fire early; 'networkidle2' (default) waits until the page has nearly stopped making network requests, and 'networkidle0' waits until it is fully idle. Use a networkidle option for pages whose content loads via background fetch/XHR after the initial load.",
			},
			"wait_selector": map[string]any{
				"type":        "string",
				"description": "Optional CSS selector to wait for before capturing the page. Use this when you know the element that holds the content you need (most precise way to wait for client-side-rendered content).",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Optional additional settle delay in milliseconds after the wait condition is met, before capturing (max 30000). Useful as a fallback when content keeps rendering briefly after the network goes idle.",
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
