package codex

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAgentPrivateKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return privateKey, base64.StdEncoding.EncodeToString(pkcs8)
}

func TestNormalizeAgentIdentityAuthFile(t *testing.T) {
	_, encoded := testAgentPrivateKey(t)
	input := map[string]any{
		"auth_mode": "agentIdentity",
		"agent_identity": map[string]any{
			"agent_runtime_id":  "runtime-1",
			"agent_private_key": encoded,
			"account_id":        "acct-1",
			"chatgpt_user_id":   "user-1",
			"email":             "a@example.com",
			"plan_type":         "plus",
			"task_id":           "task-1",
		},
		"priority": 10,
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	normalized, err := NormalizeAgentIdentityAuthFile(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(normalized, &out); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}
	if out["type"] != "codex" {
		t.Fatalf("type = %v, want codex", out["type"])
	}
	if out["auth_mode"] != AuthModeAgentIdentity {
		t.Fatalf("auth_mode = %v", out["auth_mode"])
	}
	if out["agent_runtime_id"] != "runtime-1" {
		t.Fatalf("agent_runtime_id = %v", out["agent_runtime_id"])
	}
	if out["account_id"] != "acct-1" {
		t.Fatalf("account_id = %v", out["account_id"])
	}
	if out["chatgpt_user_id"] != "user-1" {
		t.Fatalf("chatgpt_user_id = %v", out["chatgpt_user_id"])
	}
	if out["task_id"] != "task-1" {
		t.Fatalf("task_id = %v", out["task_id"])
	}
	if _, hasAccess := out["access_token"]; hasAccess {
		t.Fatal("access_token should not be present")
	}
	if _, hasRefresh := out["refresh_token"]; hasRefresh {
		t.Fatal("refresh_token should not be present")
	}
	if out["priority"] != float64(10) {
		t.Fatalf("priority = %v", out["priority"])
	}
}

func TestNormalizeAgentIdentityAuthFileLeavesNormalCodex(t *testing.T) {
	raw := []byte(`{"type":"codex","access_token":"tok","refresh_token":"rt"}`)
	out, err := NormalizeAgentIdentityAuthFile(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("expected unchanged payload")
	}
}

func TestBuildAgentAssertion(t *testing.T) {
	privateKey, encoded := testAgentPrivateKey(t)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	value, err := BuildAgentAssertion("runtime-1", "task-1", encoded, now)
	if err != nil {
		t.Fatalf("build assertion: %v", err)
	}
	if !strings.HasPrefix(value, "AgentAssertion ") {
		t.Fatalf("authorization = %q", value)
	}
	payloadB64 := strings.TrimPrefix(value, "AgentAssertion ")
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		t.Fatalf("decode assertion: %v", err)
	}
	var envelope map[string]string
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope["agent_runtime_id"] != "runtime-1" || envelope["task_id"] != "task-1" {
		t.Fatalf("envelope = %#v", envelope)
	}
	sig, err := base64.StdEncoding.DecodeString(envelope["signature"])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	message := []byte(envelope["agent_runtime_id"] + ":" + envelope["task_id"] + ":" + envelope["timestamp"])
	if !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), message, sig) {
		t.Fatal("signature verification failed")
	}
}

func TestEnsureAgentIdentityTaskRegistersWhenMissing(t *testing.T) {
	_, encoded := testAgentPrivateKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/task/register") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"task_id": "task-new"})
	}))
	defer server.Close()

	oldBase := agentIdentityAuthBaseURL
	agentIdentityAuthBaseURL = server.URL
	t.Cleanup(func() { agentIdentityAuthBaseURL = oldBase })

	metadata := map[string]any{
		"auth_mode":         AuthModeAgentIdentity,
		"agent_runtime_id":  "runtime-1",
		"agent_private_key": encoded,
		"account_id":        "acct-1",
		"chatgpt_user_id":   "user-1",
	}
	taskID, updated, err := EnsureAgentIdentityTask(context.Background(), metadata, "lock-1", server.Client())
	if err != nil {
		t.Fatalf("ensure task: %v", err)
	}
	if !updated || taskID != "task-new" {
		t.Fatalf("taskID=%q updated=%v", taskID, updated)
	}
	if metadata["task_id"] != "task-new" {
		t.Fatalf("metadata task_id = %v", metadata["task_id"])
	}
}

func TestIsAgentIdentityTaskInvalidResponse(t *testing.T) {
	if !IsAgentIdentityTaskInvalidResponse(401, []byte(`{"error":"invalid_task_id"}`)) {
		t.Fatal("expected invalid task detection")
	}
	if IsAgentIdentityTaskInvalidResponse(401, []byte(`{"error":"invalid_api_key"}`)) {
		t.Fatal("did not expect invalid task detection")
	}
}
