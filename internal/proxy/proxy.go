// Package proxy implements a sidecar HTTP proxy that intercepts Kubernetes
// probe requests and can override them when the pod is in a frozen state.
package proxy

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Config holds the settings for a Proxy.
type Config struct {
	LivenessUpstream  string // URL of the main container's liveness endpoint
	ReadinessUpstream string // URL of the main container's readiness endpoint
	StatePath         string // path to downward API file for state recovery
}

// Proxy is a minimal HTTP server that proxies probe requests to the main
// container or returns canned responses when the pod is frozen.
type Proxy struct {
	livenessUpstream  string
	readinessUpstream string
	frozen            atomic.Bool
	client            *http.Client
}

// New creates a Proxy from cfg. If cfg.StatePath is set and the file contains
// "true" (trimmed), the proxy starts in the frozen state. If the file is
// missing or unreadable, the proxy starts unfrozen.
func New(cfg Config) *Proxy {
	p := &Proxy{
		livenessUpstream:  cfg.LivenessUpstream,
		readinessUpstream: cfg.ReadinessUpstream,
		client:            &http.Client{Timeout: 2 * time.Second},
	}
	if cfg.StatePath != "" {
		if data, err := os.ReadFile(cfg.StatePath); err == nil {
			if strings.TrimSpace(string(data)) == "true" {
				p.frozen.Store(true)
			}
		}
	}
	return p
}

// Frozen reports whether the proxy is in the frozen state.
func (p *Proxy) Frozen() bool {
	return p.frozen.Load()
}

// SetFrozen sets the frozen state.
func (p *Proxy) SetFrozen(v bool) {
	p.frozen.Store(v)
}

// ProbeHandler returns an http.Handler serving GET /healthz and GET /readyz.
func (p *Proxy) ProbeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", p.handleHealthz)
	mux.HandleFunc("GET /readyz", p.handleReadyz)
	return mux
}

// ManageHandler returns an http.Handler serving POST /manage/freeze and
// POST /manage/unfreeze.
func (p *Proxy) ManageHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /manage/freeze", p.handleFreeze)
	mux.HandleFunc("POST /manage/unfreeze", p.handleUnfreeze)
	return mux
}

func (p *Proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if p.frozen.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "frozen: alive")
		return
	}
	p.proxyTo(w, p.livenessUpstream)
}

func (p *Proxy) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if p.frozen.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "frozen: not ready")
		return
	}
	p.proxyTo(w, p.readinessUpstream)
}

func (p *Proxy) handleFreeze(w http.ResponseWriter, r *http.Request) {
	p.frozen.Store(true)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "frozen")
}

func (p *Proxy) handleUnfreeze(w http.ResponseWriter, r *http.Request) {
	p.frozen.Store(false)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "unfrozen")
}

// proxyTo issues a GET to the upstream URL and copies the response status and
// body. On error it returns 503 with the error message.
func (p *Proxy) proxyTo(w http.ResponseWriter, upstream string) {
	resp, err := p.client.Get(upstream)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
