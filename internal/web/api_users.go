package web

import (
	"encoding/json"
	"net/http"

	"golang.org/x/crypto/bcrypt"

	"github.com/mtzanidakis/praktor/internal/store"
)

// registerUserAPI registers per-user and admin API routes.
func (s *Server) registerUserAPI(mux *http.ServeMux) {
	// Per-user agent CRUD
	mux.HandleFunc("GET /api/user/agents", s.listUserAgents)
	mux.HandleFunc("POST /api/user/agents", s.createUserAgent)
	mux.HandleFunc("GET /api/user/agents/{name}", s.getUserAgent)
	mux.HandleFunc("PUT /api/user/agents/{name}", s.updateUserAgent)
	mux.HandleFunc("DELETE /api/user/agents/{name}", s.deleteUserAgent)

	// Admin: user management
	mux.HandleFunc("GET /api/admin/users", s.adminListUsers)
	mux.HandleFunc("POST /api/admin/users", s.adminCreateUser)
	mux.HandleFunc("PUT /api/admin/users/{id}/password", s.adminUpdatePassword)
	mux.HandleFunc("PUT /api/admin/users/{id}/status", s.adminUpdateUserStatus)
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.adminDeleteUser)
}

// --- Per-user agent CRUD ---

func (s *Server) listUserAgents(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.UserID == "" {
		jsonError(w, "user session required", http.StatusUnauthorized)
		return
	}

	agents, err := s.store.ListAgentsByUser(sess.UserID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		out = append(out, map[string]any{
			"id":            a.ID,
			"name":          a.Name,
			"description":   a.Description,
			"model":         a.Model,
			"system_prompt": a.SystemPrompt,
		})
	}
	jsonResponse(w, out)
}

func (s *Server) createUserAgent(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.UserID == "" {
		jsonError(w, "user session required", http.StatusUnauthorized)
		return
	}

	var body struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Model        string `json:"model"`
		SystemPrompt string `json:"system_prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	// Check for duplicate name
	existing, _ := s.registry.GetAgentByUserAndName(sess.UserID, body.Name)
	if existing != nil {
		jsonError(w, "agent with this name already exists", http.StatusConflict)
		return
	}

	agent, err := s.registry.CreateAgentForUser(sess.UserID, body.Name, body.Description, body.Model, body.SystemPrompt)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]any{
		"id":            agent.ID,
		"name":          agent.Name,
		"description":   agent.Description,
		"model":         agent.Model,
		"system_prompt": agent.SystemPrompt,
	})
}

func (s *Server) getUserAgent(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.UserID == "" {
		jsonError(w, "user session required", http.StatusUnauthorized)
		return
	}

	name := r.PathValue("name")
	agent, err := s.registry.GetAgentByUserAndName(sess.UserID, name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	jsonResponse(w, map[string]any{
		"id":            agent.ID,
		"name":          agent.Name,
		"description":   agent.Description,
		"model":         agent.Model,
		"system_prompt": agent.SystemPrompt,
	})
}

func (s *Server) updateUserAgent(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.UserID == "" {
		jsonError(w, "user session required", http.StatusUnauthorized)
		return
	}

	name := r.PathValue("name")
	agent, err := s.registry.GetAgentByUserAndName(sess.UserID, name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		Model        *string `json:"model"`
		SystemPrompt *string `json:"system_prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	newName := ""
	if body.Name != nil {
		newName = *body.Name
	}
	newDesc := ""
	if body.Description != nil {
		newDesc = *body.Description
	}
	model := agent.Model
	if body.Model != nil {
		model = *body.Model
	}
	sysPrompt := agent.SystemPrompt
	if body.SystemPrompt != nil {
		sysPrompt = *body.SystemPrompt
	}

	if err := s.registry.UpdateAgentForUser(agent.ID, newName, newDesc, model, sysPrompt); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) deleteUserAgent(w http.ResponseWriter, r *http.Request) {
	sess := getSession(r)
	if sess == nil || sess.UserID == "" {
		jsonError(w, "user session required", http.StatusUnauthorized)
		return
	}

	name := r.PathValue("name")
	agent, err := s.registry.GetAgentByUserAndName(sess.UserID, name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if agent == nil {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	if err := s.registry.DeleteAgentForUser(agent.ID); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}

// --- Admin: user management ---

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	sess := getSession(r)
	if sess == nil || !sess.IsAdmin {
		jsonError(w, "admin access required", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	users, err := s.store.ListUsers()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, map[string]any{
			"id":           u.ID,
			"username":     u.Username,
			"display_name": u.DisplayName,
			"is_admin":     u.IsAdmin,
			"has_password":  u.Password != "",
			"status":       u.Status,
			"telegram_id":  u.TelegramID,
			"created_at":   u.CreatedAt,
		})
	}
	jsonResponse(w, out)
}

func (s *Server) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
		IsAdmin     bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Username == "" {
		jsonError(w, "username is required", http.StatusBadRequest)
		return
	}

	var hashedPw string
	if body.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			jsonError(w, "password hashing failed", http.StatusInternalServerError)
			return
		}
		hashedPw = string(hash)
	}

	u := &store.User{
		ID:          body.Username, // Use username as ID for web-created users
		Username:    body.Username,
		DisplayName: body.DisplayName,
		Password:    hashedPw,
		IsAdmin:     body.IsAdmin,
	}

	if err := s.store.CreateUser(u); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"is_admin": u.IsAdmin,
	})
}

func (s *Server) adminUpdatePassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	id := r.PathValue("id")
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Password == "" {
		jsonError(w, "password is required", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "password hashing failed", http.StatusInternalServerError)
		return
	}

	if err := s.store.UpdateUserPassword(id, string(hash)); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) adminUpdateUserStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	id := r.PathValue("id")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	switch body.Status {
	case "approved", "blocked", "pending":
		// valid
	default:
		jsonError(w, "status must be one of: approved, blocked, pending", http.StatusBadRequest)
		return
	}

	// Prevent self-block
	sess := getSession(r)
	if sess != nil && sess.UserID == id && body.Status != "approved" {
		jsonError(w, "cannot change your own status", http.StatusBadRequest)
		return
	}

	if err := s.store.UpdateUserStatus(id, body.Status); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If approved and user has a telegram_id, publish NATS event so bot can notify
	if body.Status == "approved" && s.nats != nil {
		user, _ := s.store.GetUser(id)
		if user != nil && user.TelegramID > 0 {
			_ = s.nats.PublishJSON("events.users.approved", map[string]any{
				"user_id":     id,
				"telegram_id": user.TelegramID,
			})
		}
	}

	jsonResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}

	id := r.PathValue("id")

	// Prevent deleting self
	sess := getSession(r)
	if sess != nil && sess.UserID == id {
		jsonError(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteUser(id); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]string{"status": "deleted"})
}
