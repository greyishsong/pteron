package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/runtime.js web/admin.html
var assets embed.FS
var bodyClose = regexp.MustCompile(`(?i)</body\s*>`)
var htmlClose = regexp.MustCompile(`(?i)</html\s*>`)

func injectRuntime(source, id string) string {
	script := `<script src="/_pteron/runtime.js" data-site-id="` + id + `"></script>`
	for _, pattern := range []*regexp.Regexp{bodyClose, htmlClose} {
		if loc := pattern.FindStringIndex(source); loc != nil {
			return source[:loc[0]] + script + source[loc[0]:]
		}
	}
	return source + script
}

type eventHub struct {
	mu      sync.Mutex
	clients map[string]map[chan string]struct{}
}

func (h *eventHub) subscribe(id string) chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[id] == nil {
		h.clients[id] = map[chan string]struct{}{}
	}
	ch := make(chan string, 1)
	h.clients[id][ch] = struct{}{}
	return ch
}
func (h *eventHub) unsubscribe(id string, ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients[id], ch)
	if len(h.clients[id]) == 0 {
		delete(h.clients, id)
	}
}
func (h *eventHub) reload(id, timestamp string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients[id] {
		select {
		case ch <- timestamp:
		default:
		}
	}
}
func (h *eventHub) close(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients[id] {
		close(ch)
	}
	delete(h.clients, id)
}

type app struct {
	store   *Store
	ip      string
	port    int
	hub     eventHub
	handler http.Handler
}

func newApp(dataDir, ip string, port int) (*app, error) {
	store, err := newStore(dataDir)
	if err != nil {
		return nil, err
	}
	a := &app{store: store, ip: ip, port: port, hub: eventHub{clients: map[string]map[chan string]struct{}{}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		a.asset(w, r, "web/admin.html", "text/html; charset=utf-8")
	})
	mux.HandleFunc("GET /_pteron/runtime.js", func(w http.ResponseWriter, r *http.Request) {
		a.asset(w, r, "web/runtime.js", "application/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET /s/{id}", a.page)
	mux.HandleFunc("GET /s/{id}/{$}", a.page)
	mux.HandleFunc("GET /api/sites", a.list)
	mux.HandleFunc("POST /api/sites", a.create)
	mux.HandleFunc("PUT /api/sites/{id}", a.update)
	mux.HandleFunc("DELETE /api/sites/{id}", a.remove)
	mux.HandleFunc("GET /api/sites/{id}/feedback", a.getFeedback)
	mux.HandleFunc("PUT /api/sites/{id}/feedback", a.saveFeedback)
	mux.HandleFunc("POST /api/sites/{id}/feedback", a.saveFeedback)
	mux.HandleFunc("DELETE /api/sites/{id}/feedback", a.clearFeedback)
	mux.HandleFunc("GET /api/sites/{id}/events", a.events)
	a.handler = mux
	return a, nil
}
func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
		// Do not let unrelated websites mutate a local publishing service.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				jsonError(w, http.StatusForbidden, "cross-origin writes are not allowed")
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				jsonError(w, http.StatusForbidden, "cross-site writes are not allowed")
				return
			}
		}
	}
	a.handler.ServeHTTP(w, r)
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func jsonError(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		jsonError(w, http.StatusNotFound, "site not found")
	case errors.Is(err, errInvalidID):
		jsonError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errConflict):
		jsonError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("storage: %v", err)
		jsonError(w, http.StatusInternalServerError, "storage operation failed")
	}
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		jsonError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}
func (a *app) asset(w http.ResponseWriter, r *http.Request, path, contentType string) {
	data, err := assets.ReadFile(path)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
func (a *app) page(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.store.mu.RLock()
	dir, err := a.store.dir(id)
	var data []byte
	if err == nil {
		data, err = os.ReadFile(filepath.Join(dir, "index.html"))
	}
	a.store.mu.RUnlock()
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, injectRuntime(string(data), id))
}
func (a *app) list(w http.ResponseWriter, r *http.Request) {
	sites, err := a.store.list(a.ip, a.port)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, sites)
}

type publishRequest struct {
	ID    string  `json:"id"`
	Title string  `json:"title"`
	HTML  *string `json:"html"`
}

func (a *app) create(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if !decode(w, r, &req) {
		return
	}
	if req.HTML == nil {
		jsonError(w, http.StatusBadRequest, "html is required")
		return
	}
	meta, err := a.store.create(req.ID, req.Title, *req.HTML)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Location", "/s/"+meta.ID)
	respond(w, http.StatusCreated, siteInfo(meta, emptyFeedback(meta.ID), a.ip, a.port))
}
func (a *app) update(w http.ResponseWriter, r *http.Request) {
	var req publishRequest
	if !decode(w, r, &req) {
		return
	}
	if req.HTML == nil {
		jsonError(w, http.StatusBadRequest, "html is required")
		return
	}
	meta, err := a.store.update(r.PathValue("id"), req.Title, *req.HTML)
	if err != nil {
		storeError(w, err)
		return
	}
	a.hub.reload(meta.ID, meta.UpdatedAt)
	a.store.mu.RLock()
	feedback, err := a.store.feedback(meta.ID)
	a.store.mu.RUnlock()
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, siteInfo(meta, feedback, a.ip, a.port))
}
func (a *app) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.store.remove(id); err != nil {
		storeError(w, err)
		return
	}
	a.hub.close(id)
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}
func (a *app) getFeedback(w http.ResponseWriter, r *http.Request) {
	a.store.mu.RLock()
	feedback, err := a.store.feedback(r.PathValue("id"))
	a.store.mu.RUnlock()
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, feedback)
}
func (a *app) saveFeedback(w http.ResponseWriter, r *http.Request) {
	var req FeedbackUpdate
	if !decode(w, r, &req) {
		return
	}
	feedback, err := a.store.saveFeedback(r.PathValue("id"), req, false)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, feedback)
}
func (a *app) clearFeedback(w http.ResponseWriter, r *http.Request) {
	feedback, err := a.store.saveFeedback(r.PathValue("id"), FeedbackUpdate{}, true)
	if err != nil {
		storeError(w, err)
		return
	}
	respond(w, http.StatusOK, feedback)
}
func (a *app) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.store.mu.RLock()
	_, err := a.store.dir(id)
	if err != nil {
		a.store.mu.RUnlock()
		storeError(w, err)
		return
	}
	ch := a.hub.subscribe(id)
	a.store.mu.RUnlock()
	defer a.hub.unsubscribe(id, ch)
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "streaming unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case timestamp, open := <-ch:
			if !open {
				return
			}
			payload, _ := json.Marshal(map[string]string{"updated_at": timestamp})
			if _, err := fmt.Fprintf(w, "event: reload\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func tailscaleIP() string {
	if iface, err := net.InterfaceByName("tailscale0"); err == nil {
		if addresses, err := iface.Addrs(); err == nil {
			for _, address := range addresses {
				ip, _, err := net.ParseCIDR(address.String())
				if err == nil && ip.To4() != nil {
					return ip.To4().String()
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output(); err == nil {
		first := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
		if ip := net.ParseIP(first); ip != nil && ip.To4() != nil {
			return ip.To4().String()
		}
	}
	return "127.0.0.1"
}
func serve(dataDir string, port int) error {
	ip := tailscaleIP()
	a, err := newApp(dataDir, ip, port)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	local, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer local.Close()
	listeners := []net.Listener{local}
	if ip != "127.0.0.1" {
		config := net.ListenConfig{Control: func(network, address string, raw syscall.RawConn) error {
			var optionErr error
			err := raw.Control(func(fd uintptr) {
				optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				if optionErr == nil {
					optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, 15 /* IP_FREEBIND */, 1)
				}
			})
			if err != nil {
				return err
			}
			return optionErr
		}}
		remote, err := config.Listen(ctx, "tcp4", fmt.Sprintf("%s:%d", ip, port))
		if err != nil {
			return err
		}
		defer remote.Close()
		listeners = append(listeners, remote)
	}
	server := &http.Server{Handler: a, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	failures := make(chan error, len(listeners))
	for _, listener := range listeners {
		log.Printf("listening on http://%s (data: %s)", listener.Addr(), dataDir)
		go func(l net.Listener) { failures <- server.Serve(l) }(listener)
	}
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
		return shutdownErr
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
