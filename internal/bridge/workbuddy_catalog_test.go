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

func workBuddyNamedModels() []object {
	return []object{
		{"id": "gpt-responses", "name": "GPT Responses", "supported_endpoints": []any{"/responses"},
			"capabilities": object{"type": "chat", "supports": object{"tool_calls": true, "streaming": true, "vision": true, "reasoning_effort": []any{"low", "medium", "high"}},
				"limits": object{"max_prompt_tokens": 64000, "max_output_tokens": 8192}}},
		{"id": defaultClaude, "name": "Claude Sonnet", "supported_endpoints": []any{"/chat/completions"}},
	}
}

func TestWorkBuddyImportsRealCatalogAndCapabilities(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	a.rawModels = append(workBuddyNamedModels(),
		object{"id": "embedding", "capabilities": object{"type": "embedding"}, "supported_endpoints": []any{"/responses"}},
		object{"id": "disabled", "policy": object{"state": "disabled"}, "supported_endpoints": []any{"/responses"}},
		object{"id": "gpt-internal", "name": "Internal only", "supported_endpoints": []any{"/responses"}},
		object{"id": "not-a-tool-model", "capabilities": object{"supports": object{"tool_calls": false}}, "supported_endpoints": []any{"/responses"}})
	if err := a.toggleWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, err := a.readWorkBuddyModels()
	if err != nil || len(doc.models) != 3 {
		t.Fatal("incorrect imported catalog size", err, len(doc.models))
	}
	var found object
	for _, entry := range doc.models {
		if obj(entry)["id"] == "modelbridge/gpt-responses" {
			found = obj(entry)
		}
	}
	if found == nil || found["name"] != "ModelBridge · GPT Responses" || found["supportsImages"] != true || found["supportsReasoning"] != true ||
		found["onlyReasoning"] != true || number(found["maxInputTokens"]) != 64000 || nested(found, "reasoning", "defaultEffort") != "medium" {
		t.Fatal("real model names/capabilities were not imported")
	}
	before, _ := os.Stat(a.workBuddyConfigPath())
	if err := a.injectWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(a.workBuddyConfigPath())
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged catalog was rewritten")
	}
	w := httptest.NewRecorder()
	a.workBuddy(w, copilotRequest("GET", "/v1/models", ""))
	var catalog object
	if json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(arr(catalog["data"])) != 3 || !strings.Contains(w.Body.String(), "modelbridge/gpt-responses") {
		t.Fatal("API directory did not expose actual models", w.Code)
	}
}

func TestWorkBuddyCatalogMigrationAndAccountChangesPreserveEdits(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	a.rawModels = workBuddyNamedModels()
	legacy := a.workBuddyEntry()
	legacy["name"] = "ModelBridge"
	original := object{"id": workBuddyModelID, "name": "User's previous model", "sequence": json.Number("9007199254740993")}
	if err := writeJSON(a.workBuddyConfigPath(), []any{legacy, object{"id": "unrelated"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(a.path("workbuddy-restore.json"), workBuddyRestore{Original: original, Managed: legacy}); err != nil {
		t.Fatal(err)
	}
	a.state.WorkBuddy = true
	if err := a.injectWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, _ := a.readWorkBuddyModels()
	for _, item := range doc.models {
		if obj(item)["id"] == "modelbridge/gpt-responses" {
			obj(item)["name"] = "User edited this"
		}
	}
	if err := a.writeWorkBuddyModels(doc); err != nil {
		t.Fatal(err)
	}
	a.rawModels = []object{{"id": "new-account-model", "name": "Other account", "supported_endpoints": []any{"/responses"}}}
	if err := a.injectWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, _ = a.readWorkBuddyModels()
	ids := map[string]object{}
	for _, item := range doc.models {
		ids[str(obj(item)["id"])] = obj(item)
	}
	if ids["modelbridge/new-account-model"] == nil || ids["modelbridge/"+defaultClaude] != nil || ids["modelbridge/gpt-responses"]["name"] != "User edited this" {
		t.Fatal("source change did not replace managed catalog or preserve user edits")
	}
	if err := a.toggleWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	doc, _ = a.readWorkBuddyModels()
	if len(doc.models) != 3 || !bytes.Equal(jsonBytes(doc.models[0]), jsonBytes(original)) {
		t.Fatal("legacy original or user-edited model was lost during restoration")
	}
}

func TestWorkBuddyWatcherUpdatesCatalogWithoutAgentIntervention(t *testing.T) {
	a := workBuddyTestApp(t, nil)
	if err := a.toggleWorkBuddy(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.rawModels = []object{{"id": "changed-source", "name": "Changed source", "supported_endpoints": []any{"/responses"}}}
	a.rawAt = time.Now()
	a.mu.Unlock()
	a.startWorkBuddyWatcher()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(a.workBuddyConfigPath())
		if err == nil && bytes.Contains(data, []byte("modelbridge/changed-source")) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("running app did not synchronize the changed source")
}

func TestWorkBuddyRoutesExactNamedResponsesModel(t *testing.T) {
	calls := 0
	a := workBuddyTestApp(t, func(r *http.Request) (*http.Response, error) {
		calls++
		var body object
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path != "/v1/responses" || body["model"] != "gpt-responses" || body["messages"] != nil || body["stream"] != false ||
			nested(body, "reasoning", "effort") != "high" || number(body["max_output_tokens"]) != 1200 {
			t.Fatal("named model or Responses request parameters changed")
		}
		input := arr(body["input"])
		if len(input) != 4 || obj(input[1])["type"] != "function_call" || obj(input[2])["type"] != "function_call_output" ||
			nested(obj(input[3]), "content") == nil || obj(arr(body["tools"])[0])["name"] != "read_file" {
			t.Fatal("tool history, tool definitions or image input were lost")
		}
		return response(200, object{"id": "response-test", "status": "completed", "output": []any{
			object{"type": "message", "content": []any{object{"type": "output_text", "text": "done"}}},
			object{"type": "function_call", "call_id": "next", "name": "read_file", "arguments": `{"path":"b"}`}},
			"usage": object{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25}}), nil
	})
	a.rawModels = workBuddyNamedModels()
	a.state.WorkBuddy = true
	body := `{"model":"modelbridge/gpt-responses","reasoning_effort":"high","max_tokens":1200,"messages":[{"role":"user","content":"read"},{"role":"assistant","content":null,"tool_calls":[{"id":"prior","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"prior","content":"file contents"},{"role":"user","content":[{"type":"text","text":"and this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,test"}}]}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`
	w := httptest.NewRecorder()
	a.workBuddy(w, copilotRequest("POST", "/v1/chat/completions", body))
	var result object
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	choice := obj(arr(result["choices"])[0])
	if w.Code != 200 || nested(choice, "message", "content") != "done" || choice["finish_reason"] != "tool_calls" ||
		len(arr(nested(choice, "message", "tool_calls"))) != 1 || number(nested(result, "usage", "total_tokens")) != 25 || calls != 1 {
		t.Fatal("Responses result was not converted to WorkBuddy's Chat format", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.workBuddy(w, copilotRequest("POST", "/v1/chat/completions", `{"model":"modelbridge/unavailable","messages":[]}`))
	if w.Code != 400 || calls != 1 {
		t.Fatal("unavailable named model silently fell back")
	}
}

func TestWorkBuddyResponsesStreamingToolsAndInterruptedStreams(t *testing.T) {
	events := []object{
		{"type": "response.created", "response": object{"id": "resp-test"}},
		{"type": "response.output_text.delta", "output_index": 0, "delta": "hello"},
		{"type": "response.output_item.added", "output_index": 1, "item": object{"type": "function_call", "call_id": "call-one", "name": "read_file", "arguments": ""}},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `{"path":`},
		{"type": "response.function_call_arguments.delta", "output_index": 1, "delta": `"test"}`},
		{"type": "response.output_item.done", "output_index": 1, "item": object{"type": "function_call", "call_id": "call-one", "name": "read_file", "arguments": `{"path":"test"}`}},
		{"type": "response.completed", "response": object{"status": "completed", "usage": object{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}},
	}
	var stream strings.Builder
	for _, event := range events {
		stream.WriteString("data: " + string(jsonBytes(event)) + "\n\n")
	}
	a := workBuddyTestApp(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String()))}, nil
	})
	a.rawModels = workBuddyNamedModels()
	a.state.WorkBuddy = true
	w := httptest.NewRecorder()
	a.workBuddy(w, copilotRequest("POST", "/v1/chat/completions", `{"model":"modelbridge/gpt-responses","stream":true,"messages":[{"role":"user","content":"read"}]}`))
	arguments, content, finish := "", "", ""
	_ = readSSE(strings.NewReader(w.Body.String()), func(e sseEvent) error {
		chunk, _ := e.data()
		if chunk == nil {
			return nil
		}
		choice := obj(arr(chunk["choices"])[0])
		content += str(nested(choice, "delta", "content"))
		for _, call := range arr(nested(choice, "delta", "tool_calls")) {
			arguments += str(nested(call, "function", "arguments"))
		}
		if choice["finish_reason"] != nil {
			finish = str(choice["finish_reason"])
		}
		return nil
	})
	if w.Code != 200 || content != "hello" || arguments != `{"path":"test"}` || finish != "tool_calls" || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("streaming text or function arguments changed", w.Body.String())
	}
	for _, source := range []string{
		"data: " + string(jsonBytes(events[1])) + "\n\n",
		`data: {"type":"response.failed","response":{"error":{"message":"quota failed"}}}` + "\n\n",
	} {
		w = httptest.NewRecorder()
		if err := workBuddyChatStream(strings.NewReader(source), w, "modelbridge/gpt-responses"); err == nil || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatal("failed or truncated stream appeared successful")
		}
	}
}
