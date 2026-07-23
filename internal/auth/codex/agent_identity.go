package codex

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

const (
	// AuthModeAgentIdentity marks Codex accounts that authenticate with AgentAssertion.
	AuthModeAgentIdentity = "agentIdentity"

	agentIdentityAuthAPIBaseURL          = "https://auth.openai.com/api/accounts"
	agentIdentityTaskRegistrationTimeout = 30 * time.Second
)

var (
	agentIdentityTaskLocks   sync.Map // map[string]*sync.Mutex
	agentIdentityAuthBaseURL = agentIdentityAuthAPIBaseURL
)

// AgentIdentityCredentials are the minimal fields required for Agent Identity accounts.
type AgentIdentityCredentials struct {
	AuthMode        string `json:"auth_mode"`
	AgentRuntimeID  string `json:"agent_runtime_id"`
	AgentPrivateKey string `json:"agent_private_key"`
	TaskID          string `json:"task_id,omitempty"`
	AccountID       string `json:"account_id"`
	ChatGPTUserID   string `json:"chatgpt_user_id"`
	Email           string `json:"email,omitempty"`
	PlanType        string `json:"plan_type,omitempty"`
}

// IsAgentIdentityAuth reports whether metadata uses Agent Identity mode.
func IsAgentIdentityAuth(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	mode := firstString(metadata, "auth_mode", "authMode")
	if strings.EqualFold(strings.TrimSpace(mode), AuthModeAgentIdentity) {
		return true
	}
	if _, ok := firstMap(metadata, "agent_identity", "agentIdentity"); ok {
		return true
	}
	return false
}

// ParseAgentIdentityCredentials extracts and validates Agent Identity fields from raw JSON/metadata.
func ParseAgentIdentityCredentials(raw map[string]any) (*AgentIdentityCredentials, error) {
	if raw == nil {
		return nil, errors.New("agent identity payload is empty")
	}
	source := raw
	if identity, ok := firstMap(raw, "agent_identity", "agentIdentity"); ok {
		source = identity
	} else if !strings.EqualFold(firstString(raw, "auth_mode", "authMode"), AuthModeAgentIdentity) {
		return nil, errors.New("not an agent identity credential")
	}

	creds := &AgentIdentityCredentials{
		AuthMode:        AuthModeAgentIdentity,
		AgentRuntimeID:  firstString(source, "agent_runtime_id", "agentRuntimeId"),
		AgentPrivateKey: firstString(source, "agent_private_key", "agentPrivateKey"),
		TaskID:          firstString(source, "task_id", "taskId"),
		AccountID: firstString(
			source,
			"account_id",
			"accountId",
			"chatgpt_account_id",
			"chatgptAccountId",
		),
		ChatGPTUserID: firstString(source, "chatgpt_user_id", "chatgptUserId", "user_id", "userId"),
		Email:         firstString(source, "email"),
		PlanType:      firstString(source, "plan_type", "planType", "chatgpt_plan_type", "chatgptPlanType"),
	}
	if creds.AgentRuntimeID == "" {
		return nil, errors.New("agent_runtime_id is required")
	}
	if creds.AgentPrivateKey == "" {
		return nil, errors.New("agent_private_key is required")
	}
	if creds.AccountID == "" {
		return nil, errors.New("account_id is required")
	}
	if creds.ChatGPTUserID == "" {
		return nil, errors.New("chatgpt_user_id is required")
	}
	if err := ValidateAgentIdentityPrivateKey(creds.AgentPrivateKey); err != nil {
		return nil, err
	}
	return creds, nil
}

// NormalizeAgentIdentityAuthFile rewrites Agent Identity payloads into CPA codex auth files.
// Non-agent payloads are returned unchanged.
func NormalizeAgentIdentityAuthFile(data []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return data, nil
	}
	if !IsAgentIdentityAuth(raw) {
		return data, nil
	}
	creds, err := ParseAgentIdentityCredentials(raw)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"type":              "codex",
		"auth_mode":         AuthModeAgentIdentity,
		"agent_runtime_id":  creds.AgentRuntimeID,
		"agent_private_key": creds.AgentPrivateKey,
		"account_id":        creds.AccountID,
		"chatgpt_user_id":   creds.ChatGPTUserID,
	}
	if creds.TaskID != "" {
		out["task_id"] = creds.TaskID
	}
	if creds.Email != "" {
		out["email"] = creds.Email
	}
	if creds.PlanType != "" {
		out["plan_type"] = creds.PlanType
	}
	// Preserve optional operational fields when present.
	for _, key := range []string{"disabled", "priority", "proxy_url", "prefix", "note", "websockets"} {
		if value, ok := raw[key]; ok {
			out[key] = value
		}
	}
	normalized, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(normalized, '\n'), nil
}

// ValidateAgentIdentityPrivateKey checks PKCS#8 Base64 Ed25519 private keys.
func ValidateAgentIdentityPrivateKey(encoded string) error {
	_, err := parseAgentIdentityPrivateKey(encoded)
	return err
}

// BuildAgentAssertion builds Authorization: AgentAssertion <base64url-json>.
func BuildAgentAssertion(runtimeID, taskID, privateKeyEncoded string, now time.Time) (string, error) {
	privateKey, err := parseAgentIdentityPrivateKey(privateKeyEncoded)
	if err != nil {
		return "", err
	}
	runtimeID = strings.TrimSpace(runtimeID)
	taskID = strings.TrimSpace(taskID)
	if runtimeID == "" || taskID == "" {
		return "", errors.New("agent identity runtime or task id is missing")
	}
	timestamp := now.UTC().Format(time.RFC3339)
	payload := []byte(runtimeID + ":" + taskID + ":" + timestamp)
	signature, err := privateKey.Sign(nil, payload, crypto.Hash(0))
	if err != nil {
		return "", errors.New("failed to sign agent assertion")
	}
	envelope := map[string]string{
		"agent_runtime_id": runtimeID,
		"task_id":          taskID,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(signature),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("failed to serialize agent assertion")
	}
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(encoded), nil
}

// EnsureAgentIdentityTask returns a usable task id, registering one when needed.
// When a new task is registered, updated is true.
func EnsureAgentIdentityTask(ctx context.Context, metadata map[string]any, lockKey string, httpClient *http.Client) (taskID string, updated bool, err error) {
	if metadata == nil {
		return "", false, errors.New("agent identity metadata is nil")
	}
	creds, err := ParseAgentIdentityCredentials(metadata)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(creds.TaskID) != "" {
		return strings.TrimSpace(creds.TaskID), false, nil
	}

	mu := agentIdentityLock(lockKey)
	mu.Lock()
	defer mu.Unlock()

	// Re-check under lock in case another request already registered.
	if existing := firstString(metadata, "task_id", "taskId"); strings.TrimSpace(existing) != "" {
		return strings.TrimSpace(existing), false, nil
	}

	newTaskID, err := registerAgentIdentityTask(ctx, creds, httpClient)
	if err != nil {
		return "", false, err
	}
	metadata["task_id"] = newTaskID
	return newTaskID, true, nil
}

// ReplaceAgentIdentityTask forces a new task registration and stores it on metadata.
func ReplaceAgentIdentityTask(ctx context.Context, metadata map[string]any, lockKey string, httpClient *http.Client) (string, error) {
	if metadata == nil {
		return "", errors.New("agent identity metadata is nil")
	}
	creds, err := ParseAgentIdentityCredentials(metadata)
	if err != nil {
		return "", err
	}
	mu := agentIdentityLock(lockKey)
	mu.Lock()
	defer mu.Unlock()

	newTaskID, err := registerAgentIdentityTask(ctx, creds, httpClient)
	if err != nil {
		return "", err
	}
	metadata["task_id"] = newTaskID
	return newTaskID, nil
}

// IsAgentIdentityTaskInvalidResponse reports task-invalid 401 style failures.
func IsAgentIdentityTaskInvalidResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	lower := strings.ToLower(string(body))
	markers := []string{
		"invalid_task_id",
		"task_not_found",
		"task_expired",
		"invalid task",
		"task not found",
		"task expired",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func registerAgentIdentityTask(ctx context.Context, creds *AgentIdentityCredentials, httpClient *http.Client) (string, error) {
	if creds == nil {
		return "", errors.New("agent identity credentials are nil")
	}
	privateKey, err := parseAgentIdentityPrivateKey(creds.AgentPrivateKey)
	if err != nil {
		return "", err
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature, err := privateKey.Sign(nil, []byte(creds.AgentRuntimeID+":"+timestamp), crypto.Hash(0))
	if err != nil {
		return "", errors.New("failed to sign agent task registration")
	}
	body, err := json.Marshal(map[string]string{
		"timestamp": timestamp,
		"signature": base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		return "", errors.New("failed to serialize agent task registration")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: agentIdentityTaskRegistrationTimeout}
	}
	url := strings.TrimRight(agentIdentityAuthBaseURL, "/") + "/v1/agent/" + creds.AgentRuntimeID + "/task/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("failed to build agent task registration request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent task registration request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("agent task registration returned status %d", resp.StatusCode)
	}
	var result struct {
		TaskID               string `json:"task_id"`
		TaskIDCamel          string `json:"taskId"`
		EncryptedTaskID      string `json:"encrypted_task_id"`
		EncryptedTaskIDCamel string `json:"encryptedTaskId"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return "", errors.New("agent task registration response is invalid")
	}
	if taskID := strings.TrimSpace(result.TaskID); taskID != "" {
		return taskID, nil
	}
	if taskID := strings.TrimSpace(result.TaskIDCamel); taskID != "" {
		return taskID, nil
	}
	encrypted := strings.TrimSpace(result.EncryptedTaskID)
	if encrypted == "" {
		encrypted = strings.TrimSpace(result.EncryptedTaskIDCamel)
	}
	if encrypted == "" {
		return "", errors.New("agent task registration response omitted task id")
	}
	return decryptAgentTaskID(privateKey, encrypted)
}

func decryptAgentTaskID(privateKey ed25519.PrivateKey, encoded string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", errors.New("encrypted agent task id is not valid base64")
	}
	seed := privateKey.Seed()
	digest := sha512.Sum512(seed)
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	curvePublicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		return "", errors.New("failed to derive agent identity decryption key")
	}
	var curvePublic [32]byte
	copy(curvePublic[:], curvePublicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", errors.New("failed to decrypt encrypted agent task id")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", errors.New("decrypted agent task id is empty")
	}
	return taskID, nil
}

func parseAgentIdentityPrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		// Accept raw URL encoding as a convenience.
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, errors.New("agent_private_key must be base64-encoded PKCS#8")
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(raw)
	if err != nil {
		return nil, errors.New("agent_private_key must be PKCS#8")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("agent_private_key must be an Ed25519 private key")
	}
	return privateKey, nil
}

func agentIdentityLock(lockKey string) *sync.Mutex {
	key := strings.TrimSpace(lockKey)
	if key == "" {
		key = "default"
	}
	actual, _ := agentIdentityTaskLocks.LoadOrStore(key, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	if mu == nil {
		mu = &sync.Mutex{}
		agentIdentityTaskLocks.Store(key, mu)
	}
	return mu
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			switch typed := value.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func firstMap(raw map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if typed, okMap := value.(map[string]any); okMap {
				return typed, true
			}
		}
	}
	return nil, false
}
