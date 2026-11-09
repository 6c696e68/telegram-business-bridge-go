package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// healthHandler serves GET /healthz with a non-sensitive JSON snapshot. Bind
// it to a loopback address unless a reverse proxy provides access control.
func (b *Bridge) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		st := b.snapshotRuntime()
		status := "ok"
		if st.DBError {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":        status,
			"paused":        st.Paused,
			"rules_on":      st.RulesOn,
			"replied_today": st.DayCount,
			"chats_ram":     st.ChatsRAM,
			"chats_db":      st.ChatsDB,
			"db_bytes":      st.DBBytes,
			"rss_bytes":     st.RSSBytes,
			"heap_bytes":    st.HeapBytes,
			"goroutines":    st.Goroutines,
		})
	})
	return mux
}

// startHealth runs the optional health endpoint until ctx is cancelled.
func (b *Bridge) startHealth(ctx context.Context, addr string) {
	if addr == "" {
		return
	}
	if !loopbackAddr(addr) {
		logf("WARN", "health endpoint is bound to a non-loopback address; protect it with a firewall or reverse proxy")
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           b.healthHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logf("WARN", "health endpoint disabled: %s", err)
	}
}

// loopbackAddr reports whether addr only accepts loopback connections.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}
