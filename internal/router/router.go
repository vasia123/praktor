package router

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mtzanidakis/praktor/internal/config"
	"github.com/mtzanidakis/praktor/internal/registry"
)

type Orchestrator interface {
	RouteQuery(ctx context.Context, agentID string, message string) (string, error)
}

type Router struct {
	registry     *registry.Registry
	defaultAgent string
	orch         Orchestrator
}

func New(reg *registry.Registry, cfg config.RouterConfig) *Router {
	return &Router{
		registry:     reg,
		defaultAgent: cfg.DefaultAgent,
	}
}

func (r *Router) SetOrchestrator(orch Orchestrator) {
	r.orch = orch
}

// Route routes a message to the correct agent (legacy: global agent lookup).
func (r *Router) Route(ctx context.Context, message string) (agentID string, cleanedMessage string, err error) {
	// 0. Check for @swarm prefix
	if strings.HasPrefix(message, "@swarm ") {
		return "swarm", strings.TrimPrefix(message, "@swarm "), nil
	}

	// 1. Check for @agent_name prefix
	if strings.HasPrefix(message, "@") {
		parts := strings.SplitN(message, " ", 2)
		name := strings.TrimPrefix(parts[0], "@")
		if _, ok := r.registry.GetDefinition(name); ok {
			cleaned := ""
			if len(parts) > 1 {
				cleaned = parts[1]
			}
			return name, cleaned, nil
		}
		// Fall through to smart routing
	}

	// 2. Try smart routing via default agent
	if r.orch != nil && r.defaultAgent != "" {
		descs := r.registry.AgentDescriptions()
		if len(descs) > 1 {
			routedAgent, routeErr := r.orch.RouteQuery(ctx, r.defaultAgent, buildRoutingPrompt(descs, message))
			if routeErr != nil {
				slog.Debug("route query failed, using default agent", "error", routeErr)
			} else {
				// Validate the routed agent exists
				routedAgent = strings.TrimSpace(routedAgent)
				if _, ok := r.registry.GetDefinition(routedAgent); ok {
					return routedAgent, message, nil
				}
				slog.Debug("route query returned unknown agent, using default", "agent", routedAgent)
			}
		}
	}

	// 3. Fall back to default agent
	if r.defaultAgent == "" {
		return "", message, fmt.Errorf("no default agent configured")
	}
	return r.defaultAgent, message, nil
}

// RouteForUser routes a message for a specific user, looking up agents from the DB.
func (r *Router) RouteForUser(ctx context.Context, userID, message string) (agentID string, agentName string, cleanedMessage string, err error) {
	// 0. Check for @swarm prefix
	if strings.HasPrefix(message, "@swarm ") {
		return "swarm", "swarm", strings.TrimPrefix(message, "@swarm "), nil
	}

	// 1. Check for @agent_name prefix
	if strings.HasPrefix(message, "@") {
		parts := strings.SplitN(message, " ", 2)
		name := strings.TrimPrefix(parts[0], "@")

		// Look up in user's agents
		ag, err := r.registry.GetAgentByUserAndName(userID, name)
		if err == nil && ag != nil {
			cleaned := ""
			if len(parts) > 1 {
				cleaned = parts[1]
			}
			return ag.ID, ag.Name, cleaned, nil
		}

		// Also check global YAML agents
		if _, ok := r.registry.GetDefinition(name); ok {
			cleaned := ""
			if len(parts) > 1 {
				cleaned = parts[1]
			}
			return name, name, cleaned, nil
		}
		// Fall through to default
	}

	// 2. Find default agent for user (first agent)
	agents, err := r.registry.ListByUser(userID)
	if err != nil || len(agents) == 0 {
		// Fall back to global default agent
		if r.defaultAgent != "" {
			return r.defaultAgent, r.defaultAgent, message, nil
		}
		return "", "", message, fmt.Errorf("no agents configured for user %s", userID)
	}

	// Use first agent as default
	return agents[0].ID, agents[0].Name, message, nil
}

func (r *Router) DefaultAgent() string {
	return r.defaultAgent
}

// SetDefaultAgent updates the default agent used for routing.
func (r *Router) SetDefaultAgent(agent string) {
	r.defaultAgent = agent
}

func buildRoutingPrompt(descs map[string]string, message string) string {
	var sb strings.Builder
	sb.WriteString("You are a message router. Given the user's message, determine which agent should handle it.\n\n")
	sb.WriteString("Available agents:\n")
	for name, desc := range descs {
		fmt.Fprintf(&sb, "- %s: %s\n", name, desc)
	}
	sb.WriteString("\nUser message: ")
	sb.WriteString(message)
	sb.WriteString("\n\nRespond with ONLY the agent name, nothing else.")
	return sb.String()
}
