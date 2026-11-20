//go:build !appstore

package bridge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func workBuddyTestApp(t *testing.T, transport roundTripFunc) *App {
	t.Helper()
	a := testApp(t, transport)
	a.state = session{Token: "workbuddy-test-account", Username: "example"}
	a.bearer, a.expiry = "workbuddy-test-bearer", float64(time.Now().Add(time.Hour).Unix())
	a.rawModels = []object{
		{"id": "responses-only", "supported_endpoints": []any{"/responses"}},
		{"id": "disabled-chat", "policy": object{"state": "disabled"}, "supported_endpoints": []any{"/chat/completions"}},
		{"id": defaultClaude, "supported_endpoints": []any{"/chat/completions"}},
	}
	a.rawAt = time.Now()
	return a
}

func TestWorkBuddyToggleRestoresOnlyManagedEntry(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(map[bool]string{false: "array", true: "object"}[envelope], func(t *testing.T) {
			a := workBuddyTestApp(t, nil)
			original := object{"id": workBuddyModelID, "url": "https://original.example/v1", "apiKey": "original-test-key", "extra": json.Number("9007199254740993")}
			other := object{"id": "other", "apiKey": "other-test-key", "extra": json.Number("9007199254740993")}
			var initial any = []any{original, other}
			if envelope {
				initial = object{"models": initial, "extra": "preserve me"}
			}
			if err := writeJSON(a.workBuddyConfigPath(), initial); err != nil {
				t.Fatal(err)
			}
			if result := controlCall(a, "/api/toggle-workbuddy", object{}); result.Code != 200 {
				t.Fatal(result.Body.String())
			}
			s := a.snapshot()
			if !s.WorkBuddy || s.Copilot || s.Codex || s.Claude {
				t.Fatal("WorkBuddy switch changed another client's state")
			}
			doc, err := a.readWorkBuddyModels()
			if err != nil || len(doc.models) != 4 || !bytes.Equal(jsonBytes(doc.models[0]), jsonBytes(original)) {
				t.Fatal("WorkBuddy entry was not configured", err)
			}
			if info, err := os.Stat(a.path("workbuddy-restore.json")); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("restoration containing original credentials must be private", err)
			}
			// Simulate an unrelated edit in WorkBuddy while the bridge is enabled.
			obj(doc.models[1])["name"] = "Edited later"
			doc.models = append(doc.models, object{"id": "new-model"})
			if err = a.writeWorkBuddyModels(doc); err != nil {
				t.Fatal(err)
			}
			if result := controlCall(a, "/api/toggle-workbuddy", object{}); result.Code != 200 {
				t.Fatal(result.Body.String())
			}
			doc, err = a.readWorkBuddyModels()
			if err != nil || a.snapshot().WorkBuddy || len(doc.models) != 3 || !bytes.Equal(jsonBytes(doc.models[0]), jsonBytes(original)) ||
				obj(doc.models[1])["name"] != "Edited later" || obj(doc.models[1])["extra"] != json.Number("9007199254740993") {
				t.Fatal("restoration lost original provider or subsequent edits", err)
			}
			if envelope && doc.root["extra"] != "preserve me" {
				t.Fatal("lost document metadata")
			}
		})
	}
}

func TestWorkBuddyRestartAndDisconnect(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	if result := controlCall(a, "/api/toggle-workbuddy", object{}); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	// An existing journal makes repeated startup injection idempotent.
	if err := a.injectWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.workBuddyConfigPath()); !os.IsNotExist(err) {
		t.Fatal("quit left an injected entry behind", err)
	}
	reloaded, err := New(a.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.Stop() })
	reloaded.rawModels, reloaded.rawAt = a.rawModels, time.Now()
	reloaded.initialize()
	if !reloaded.snapshot().WorkBuddy {
		t.Fatal("restart lost the enabled preference")
	}
	doc, err := reloaded.readWorkBuddyModels()
	if err != nil || len(doc.models) != 2 {
		t.Fatal("restart did not restore WorkBuddy configuration", err)
	}
	status := controlCall(reloaded, "/api/status", nil)
	var value object
	if json.Unmarshal(status.Body.Bytes(), &value) != nil || value["workbuddyEnabled"] != true || value["workbuddyURL"] != "http://127.0.0.1:18926/v1" {
		t.Fatal("missing WorkBuddy status")
	}
	if err := reloaded.disconnect(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.workBuddyConfigPath()); !os.IsNotExist(err) {
		t.Fatal("disconnect left an injected entry behind", err)
	}
}

func TestWorkBuddyPreservesEditedManagedEntry(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	if err := a.toggleWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, _ := a.readWorkBuddyModels()
	obj(doc.models[0])["apiKey"] = "new-user-test-key"
	if err := a.writeWorkBuddyModels(doc); err != nil {
		t.Fatal(err)
	}
	if err := a.toggleWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, err := a.readWorkBuddyModels()
	if err != nil || len(doc.models) != 1 || obj(doc.models[0])["apiKey"] != "new-user-test-key" {
		t.Fatal("disabling overwrote the user's newer entry", err)
	}
}

func TestWorkBuddyRejectsBadConfigAndRollsBackFailedSave(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	data := []byte(`{"unexpected":true}`)
	if err := writeAtomic(a.workBuddyConfigPath(), data); err != nil {
		t.Fatal(err)
	}
	if err := a.toggleWorkBuddy(); err == nil || a.snapshot().WorkBuddy {
		t.Fatal("invalid config enabled the bridge")
	}
	current, _ := os.ReadFile(a.workBuddyConfigPath())
	if !bytes.Equal(data, current) {
		t.Fatal("invalid configuration was overwritten")
	}
	_ = os.Remove(a.workBuddyConfigPath())
	// A directory at the session file path forces persistence to fail after injection.
	if err := os.MkdirAll(a.path("session.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.toggleWorkBuddy(); err == nil || a.snapshot().WorkBuddy {
		t.Fatal("failed persistence did not roll back enablement")
	}
	if _, err := os.Stat(a.workBuddyConfigPath()); !os.IsNotExist(err) {
		t.Fatal("failed persistence left WorkBuddy pointed at a disabled endpoint", err)
	}
}

func TestWorkBuddyProxyStreamingToolsAndIndependentSwitch(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"ready\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "github", true: "local"}[local], func(t *testing.T) {
			calls := 0
			a := workBuddyTestApp(t, func(r *http.Request) (*http.Response, error) {
				calls++
				var body object
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["model"] != defaultClaude || r.URL.Path != "/chat/completions" || body["stream"] != true || len(arr(body["tools"])) != 1 || len(arr(body["messages"])) != 3 ||
					r.Header.Get("Authorization") != "Bearer workbuddy-test-bearer" {
					t.Fatal("WorkBuddy lost routing, tool history or upstream authentication")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})
			if local {
				engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var body object
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["model"] != "qwen3-4b" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer local-test-secret" || len(arr(body["tools"])) != 1 {
						t.Error("local model was not routed through the native engine")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, stream)
				}))
				defer engine.Close()
				a.state.LocalModel = "qwen3-4b"
				a.local.endpoint, a.local.secret = engine.URL, "local-test-secret"
			}
			a.state.WorkBuddy = true
			body := `{"model":"modelbridge","stream":true,"messages":[{"role":"user","content":"ready"},{"role":"assistant","tool_calls":[{"id":"prior","type":"function","function":{"name":"ready","arguments":"{}"}}]},{"role":"tool","tool_call_id":"prior","content":"ok"}],"tools":[{"type":"function","function":{"name":"ready","parameters":{"type":"object"}}}]}`
			selected := workBuddyModelPrefix + defaultClaude
			if local {
				selected = workBuddyModelPrefix + "qwen3-4b"
			}
			body = strings.Replace(body, `"modelbridge"`, `"`+selected+`"`, 1)
			w := httptest.NewRecorder()
			a.workBuddy(w, copilotRequest("POST", "/v1/chat/completions", body))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"tool_calls"`) || !strings.Contains(w.Body.String(), "[DONE]") || calls != 1 {
				t.Fatal("stream or tool call was lost", w.Code, w.Body.String())
			}
			w = httptest.NewRecorder()
			a.copilotApp(w, copilotRequest("POST", "/v1/chat/completions", body))
			if w.Code != 401 || calls != 1 {
				t.Fatal("WorkBuddy enabled the Copilot endpoint")
			}
			for _, fixture := range []struct {
				method, path, key, origin string
				code                      int
			}{
				{"GET", "/v1/models", "PROXY_MANAGED", "", 200},
				{"POST", "/v1/chat/completions", "wrong", "", 401},
				{"POST", "/v1/chat/completions", "PROXY_MANAGED", "https://foreign.example", 403},
				{"POST", "/v1/responses", "PROXY_MANAGED", "", 404},
			} {
				r := copilotRequest(fixture.method, fixture.path, body)
				r.Header.Set("Authorization", "Bearer "+fixture.key)
				r.Header.Set("Origin", fixture.origin)
				w = httptest.NewRecorder()
				a.workBuddy(w, r)
				if w.Code != fixture.code {
					t.Fatal("WorkBuddy endpoint boundary failed", fixture, w.Code)
				}
			}
			a.state.WorkBuddy = false
			w = httptest.NewRecorder()
			a.workBuddy(w, copilotRequest("POST", "/v1/chat/completions", body))
			if w.Code != 401 || calls != 1 {
				t.Fatal("disabled WorkBuddy endpoint accepted inference")
			}
		})
	}
}

func TestWorkBuddyRejectsUnavailableModels(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	a.rawModels = a.rawModels[1:2]
	if err := a.toggleWorkBuddy(); err == nil || a.snapshot().WorkBuddy {
		t.Fatal("disabled model was accepted for WorkBuddy")
	}
}
