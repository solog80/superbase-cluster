package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// ──────────────── USER MANAGEMENT WRITES (Supabase master, Firebase backup) ────────────────
// Mirrors functions/src/userManagement.js (createUser/updateUserRole/deleteUser) in the mesh.
// Firebase Auth stays the identity provider (create/role/delete), Supabase `users` is the data
// master, and Firestore mirrors the user doc. The read path (getUsersPaginated/searchUsers)
// lives in main.go.

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// firebaseAdminToken loads the service account and returns a cloud-platform
// access token + the Firebase project id.
func (s *server) firebaseAdminToken(ctx context.Context) (token, projectID string, err error) {
	sa, err := loadFCMSA()
	if err != nil {
		return "", "", err
	}
	if sa.ProjectID == "" {
		return "", "", fmt.Errorf("service account missing project_id")
	}
	tok, err := s.serviceAccountToken(ctx, sa, cloudPlatformScope)
	return tok, sa.ProjectID, err
}

// firebaseAuthRPC posts to the Firebase Auth admin API (identitytoolkit).
func (s *server) firebaseAuthRPC(ctx context.Context, token, projectID, method string, body any) (map[string]any, error) {
	u := "https://identitytoolkit.googleapis.com/v1/projects/" + url.PathEscape(projectID) + method
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("firebase auth %s: %d %s", method, resp.StatusCode, string(b))
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// firestoreSetUser mirrors a user doc to Firestore users/{uid}.
func (s *server) firestoreSetUser(ctx context.Context, token, projectID, uid string, fields map[string]any) error {
	doc := map[string]any{}
	for k, v := range fields {
		doc[k] = firestoreValue(v)
	}
	payload, _ := json.Marshal(map[string]any{"fields": doc})
	u := "https://firestore.googleapis.com/v1/projects/" + url.PathEscape(projectID) +
		"/databases/(default)/documents/users/" + url.PathEscape(uid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("firestore set user %s: %d %s", uid, resp.StatusCode, string(b))
	}
	return nil
}

func (s *server) firestoreDeleteUser(ctx context.Context, token, projectID, uid string) error {
	u := "https://firestore.googleapis.com/v1/projects/" + url.PathEscape(projectID) +
		"/databases/(default)/documents/users/" + url.PathEscape(uid)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("firestore delete user %s: %d %s", uid, resp.StatusCode, string(b))
	}
	return nil
}

func firestoreValue(v any) map[string]any {
	switch t := v.(type) {
	case string:
		return map[string]any{"stringValue": t}
	case bool:
		return map[string]any{"booleanValue": t}
	case int:
		return map[string]any{"integerValue": fmt.Sprintf("%d", t)}
	case float64:
		return map[string]any{"doubleValue": t}
	case time.Time:
		return map[string]any{"timestampValue": t.UTC().Format(time.RFC3339)}
	}
	return map[string]any{"nullValue": nil}
}

// handleCreateUser mirrors createUser: creates the auth user, sets the role
// claim, upserts Supabase users and mirrors Firestore.
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if body.Email == "" || body.Password == "" || body.Role == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email, password, and role are required"})
		return
	}
	if body.Role != "admin" && body.Role != "moderator" && body.Role != "editor" && body.Role != "viewer" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid role"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, projectID, err := s.firebaseAdminToken(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	res, err := s.firebaseAuthRPC(ctx, token, projectID, "/accounts", map[string]any{
		"email": body.Email, "password": body.Password,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	uid, _ := res["localId"].(string)
	if uid == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "no localId from auth"})
		return
	}

	claims := fmt.Sprintf(`{"role":"%s"}`, body.Role)
	if _, err := s.firebaseAuthRPC(ctx, token, projectID, "/accounts:update", map[string]any{
		"localId": uid, "customAttributes": claims,
	}); err != nil {
		log.Printf("[createUser] set claims failed: %v", err)
	}

	_ = s.upsertSupabaseUserRow(ctx, uid, map[string]any{
		"email": body.Email, "role": body.Role, "is_admin": body.Role == "admin",
	})
	if err := s.firestoreSetUser(ctx, token, projectID, uid, map[string]any{
		"email": body.Email, "role": body.Role, "isAdmin": body.Role == "admin",
		"uid": uid, "createdAt": time.Now().UTC(),
	}); err != nil {
		log.Printf("[createUser] firestore mirror failed: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "uid": uid, "message": "User created"})
}

// handleUpdateUserRole mirrors updateUserRole.
func (s *server) handleUpdateUserRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID  string `json:"userId"`
		NewRole string `json:"newRole"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.UserID == "" || body.NewRole == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId and newRole are required"})
		return
	}
	if body.NewRole != "admin" && body.NewRole != "moderator" && body.NewRole != "editor" && body.NewRole != "viewer" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid role"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, projectID, err := s.firebaseAdminToken(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	claims := fmt.Sprintf(`{"role":"%s"}`, body.NewRole)
	if _, err := s.firebaseAuthRPC(ctx, token, projectID, "/accounts:update", map[string]any{
		"localId": body.UserID, "customAttributes": claims,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	_ = s.restPatch(ctx, "users", "id=eq."+url.QueryEscape(body.UserID),
		map[string]any{"role": body.NewRole, "is_admin": body.NewRole == "admin", "updated_at": time.Now().UTC().Format(time.RFC3339)})
	if err := s.firestoreSetUser(ctx, token, projectID, body.UserID, map[string]any{
		"role": body.NewRole, "isAdmin": body.NewRole == "admin", "updatedAt": time.Now().UTC(),
	}); err != nil {
		log.Printf("[updateUserRole] firestore mirror failed: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Role updated"})
}

// handleDeleteUser mirrors deleteUser.
func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if body.UserID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, projectID, err := s.firebaseAdminToken(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if _, err := s.firebaseAuthRPC(ctx, token, projectID, "/accounts:delete", map[string]any{
		"localId": body.UserID,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	_ = s.restDelete(ctx, "users", "id=eq."+url.QueryEscape(body.UserID))
	if err := s.firestoreDeleteUser(ctx, token, projectID, body.UserID); err != nil {
		log.Printf("[deleteUser] firestore mirror failed: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "User deleted"})
}

// upsertSupabaseUserRow inserts or patches a Supabase users row by id.
func (s *server) upsertSupabaseUserRow(ctx context.Context, uid string, data map[string]any) error {
	raw, _, err := s.doRest(ctx, "users", url.Values{"select": {"id"}, "id": {"eq." + url.QueryEscape(uid)}})
	if err != nil {
		return err
	}
	var existing []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &existing) == nil && len(existing) > 0 {
		return s.restPatch(ctx, "users", "id=eq."+url.QueryEscape(uid), data)
	}
	row := map[string]any{"id": uid}
	for k, v := range data {
		row[k] = v
	}
	row["created_at"] = time.Now().UTC().Format(time.RFC3339)
	return s.restPostRow(ctx, "users", row)
}
