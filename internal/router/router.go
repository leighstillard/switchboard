// Package router implements the rule engine that matches ingested events
// to destinations and coordinates message flow between Slack and jcode.
package router

import (
	"context"
	"log/slog"
	"sync"

	"github.com/format5/switchboard/internal/config"
	"github.com/format5/switchboard/internal/ingest"
	"github.com/format5/switchboard/internal/jcode"
	"github.com/format5/switchboard/internal/slack"
	"github.com/format5/switchboard/internal/store"
)

// Router evaluates routing rules and dispatches events to their destinations.
type Router struct {
	routes []config.RouteConfig
	slack  *slack.Edge
	jcode  *jcode.Client
	store  *store.Store
	ingest *ingest.Server
	mu     sync.RWMutex
}

// New creates a new Router with the given routes and component references.
func New(routes []config.RouteConfig, se *slack.Edge, jc *jcode.Client, st *store.Store, ing *ingest.Server) *Router {
	return &Router{
		routes: routes,
		slack:  se,
		jcode:  jc,
		store:  st,
		ingest: ing,
	}
}

// Run starts the router event loop. Blocks until ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	slog.Info("router started", "routes", len(r.routes))

	for {
		select {
		case <-ctx.Done():
			slog.Info("router stopped")
			return
		case evt := <-r.ingest.Events():
			r.dispatch(ctx, evt)
		}
	}
}

// Reload hot-swaps the routing rules.
func (r *Router) Reload(routes []config.RouteConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = routes
	slog.Info("router rules reloaded", "routes", len(routes))
}

func (r *Router) dispatch(ctx context.Context, evt ingest.Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, route := range r.routes {
		if r.matches(route, evt) {
			slog.Info("event routed",
				"source", evt.Source,
				"destination", route.Destination.ChannelID,
			)
			// TODO: format message from template and send to destination
			_ = r.slack.SendMessage(ctx, route.Destination.ChannelID, string(evt.Payload), "")
			return
		}
	}

	slog.Debug("no route matched", "source", evt.Source)
}

func (r *Router) matches(route config.RouteConfig, evt ingest.Event) bool {
	if route.Source != evt.Source {
		return false
	}
	if route.Match.EventType != "" && route.Match.EventType != evt.EventType {
		return false
	}
	// TODO: implement more match criteria (repo, branch, etc.)
	return true
}
