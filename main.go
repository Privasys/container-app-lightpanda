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
}

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

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	content, err := fetchURL(ctx, req.URL, format)
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

func fetchURL(ctx context.Context, url, format string) (string, error) {
	args := []string{"fetch", "--dump", format, url}

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
