// Package mcpserver exposes ReadQuest's domain as MCP tools over Streamable
// HTTP, so ClickHouse Cloud's hosted LibreChat ("ClickHouse Agent") can drive
// the app through conversation.
//
// This is the whole user interface. There is no web front end: a student logs
// a session by saying so, and a teacher asks who needs help in plain language.
// The CLI exists for development and as a fallback if the hosted chat or the
// tunnel misbehaves.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"readquest/internal/app"
)

const (
	serverName    = "readquest"
	serverVersion = "0.1.0"

	// MCP endpoint path. LibreChat is given the ngrok URL plus this path.
	endpointPath = "/mcp"

	shutdownGrace = 10 * time.Second
)

type Server struct {
	app  *app.App
	mcp  *server.MCPServer
	http *server.StreamableHTTPServer
}

func New(a *app.App) *Server {
	mcpSrv := server.NewMCPServer(serverName, serverVersion)

	s := &Server{app: a, mcp: mcpSrv}
	s.registerTools()

	// Stateless: every request carries everything needed to serve it, with no
	// server-side session to establish first. That matters here because the
	// connection crosses an ngrok tunnel — a dropped tunnel would strand a
	// stateful session, where stateless simply reconnects.
	s.http = server.NewStreamableHTTPServer(mcpSrv,
		server.WithEndpointPath(endpointPath),
		server.WithStateLess(true),
	)
	return s
}

// ListenAndServe runs until ctx is cancelled, then drains in flight requests.
func (s *Server) ListenAndServe(ctx context.Context, addr, apiKey string) error {
	mux := http.NewServeMux()

	// The MCP endpoint sits behind auth; /healthz deliberately does not, so
	// the tunnel can be checked without a key.
	mux.Handle(endpointPath, requireAPIKey(apiKey, s.http))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("mcp server listening",
			"addr", addr, "endpoint", endpointPath, "tools", len(toolNames))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down mcp server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		return nil
	}
}
