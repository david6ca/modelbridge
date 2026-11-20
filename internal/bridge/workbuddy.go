package bridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const workBuddyModelID = "modelbridge"

type workBuddyModelRestore struct {
	Original object `json:"original,omitempty"`
	Managed  object `json:"managed"`
	Previous object `json:"previous,omitempty"`
}
type workBuddyRestore struct {
	// Original/Managed are read only to migrate the first single-model release.
	Original object                  `json:"original,omitempty"`
	Managed  object                  `json:"managed,omitempty"`
	Models   []workBuddyModelRestore `json:"models,omitempty"`
	Created  bool                    `json:"created"`
}

// WorkBuddy accepts both a model array and an object containing a models array.
// Keep the surrounding document and other providers intact in either format.
type workBuddyDocument struct {
	root   object
	models []any
	data   []byte
}

func (a *App) workBuddyConfigPath() string {
	return filepath.Join(a.options.Home, ".workbuddy", "models.json")
}

func (a *App) readWorkBuddyModels() (workBuddyDocument, error) {
	doc := workBuddyDocument{models: []any{}}
	data, err := readOptional(a.workBuddyConfigPath())
	if err != nil || data == nil {
		return doc, err
	}
	doc.data = data
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return doc, errors.New("Invalid WorkBuddy models.json; existing configuration was preserved")
	}
	switch v := value.(type) {
	case []any:
		doc.models = v
	case map[string]any:
		doc.root = v
		var ok bool
		doc.models, ok = v["models"].([]any)
		if !ok {
			return doc, errors.New("WorkBuddy models.json must contain a models array")
		}
	default:
		return doc, errors.New("WorkBuddy models.json must be a model array")
	}
	return doc, nil
}

func (a *App) writeWorkBuddyModels(doc workBuddyDocument) error {
	current, err := readOptional(a.workBuddyConfigPath())
	if err != nil {
		return err
	}
	if !bytes.Equal(current, doc.data) {
		return errors.New("WorkBuddy configuration changed; try again")
	}
	if doc.root != nil {
		doc.root["models"] = doc.models
		return writeJSON(a.workBuddyConfigPath(), doc.root)
	}
	return writeJSON(a.workBuddyConfigPath(), doc.models)
}

const workBuddyModelPrefix = "modelbridge/"

func (a *App) workBuddyCatalog(ctx context.Context) ([]object, error) {
	raw, err := a.discover(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("WorkBuddy model discovery: %w", err)
	}
	seen := map[string]bool{}
	models := []object{}
	for _, model := range raw {
		id := str(model["id"])
		policy, kind := str(nested(model, "policy", "state")), str(nested(model, "capabilities", "type"))
		endpoints := stringsOf(model["supported_endpoints"])
		if id == "" || seen[id] || internalModelPattern.MatchString(id+" "+str(model["name"])+" "+str(model["description"])) ||
			(policy != "" && policy != "enabled") || (kind != "" && kind != "chat") || model["model_picker_enabled"] == false ||
			nested(model, "capabilities", "supports", "tool_calls") == false || nested(model, "capabilities", "supports", "streaming") == false ||
			(!contains(endpoints, "/chat/completions") && !contains(endpoints, "/responses")) {
			continue
		}
		seen[id] = true
		models = append(models, model)
	}
	sort.SliceStable(models, func(i, j int) bool { return modelLess(str(models[i]["id"]), str(models[j]["id"])) })
	if len(models) == 0 {
		return nil, errors.New("This account has no compatible WorkBuddy models")
	}
	return models, nil
}

func (a *App) workBuddyEntries(models []object) []object {
	entries := []object{}
	for _, model := range models {
		id, name := str(model["id"]), str(model["name"])
		if name == "" {
			name = id
		}
		entry := object{"id": workBuddyModelPrefix + id, "name": name, "vendor": "custom",
			"url": a.WorkBuddyURL() + "/chat/completions", "apiKey": "PROXY_MANAGED",
			"supportsToolCall": true, "supportsImages": false, "supportsReasoning": false,
			"useCustomProtocol": true}
		supports := obj(nested(model, "capabilities", "supports"))
		entry["supportsImages"] = supports["vision"] == true
		efforts := stringsOf(supports["reasoning_effort"])
		if len(efforts) > 0 {
			entry["supportsReasoning"] = true
			available := []string{}
			for _, effort := range efforts {
				if contains([]string{"minimal", "low", "medium", "high", "xhigh", "max"}, effort) {
					available = append(available, effort)
				}
			}
			if len(available) > 0 {
				entry["reasoning"] = object{"supportedEfforts": available, "defaultEffort": chooseValue("reasoning_effort", "medium", available), "canDisableThinking": contains(efforts, "none")}
			}
			entry["onlyReasoning"] = !contains(efforts, "none")
		}
		for from, to := range map[string]string{"max_prompt_tokens": "maxInputTokens", "max_output_tokens": "maxOutputTokens"} {
			if value := number(nested(model, "capabilities", "limits", from)); value > 0 {
				entry[to] = value
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func workBuddyManaged(item any, record workBuddyModelRestore) bool {
	return bytes.Equal(jsonBytes(item), jsonBytes(record.Managed)) || (record.Previous != nil && bytes.Equal(jsonBytes(item), jsonBytes(record.Previous)))
}

func (a *App) workBuddyJournal() (*workBuddyRestore, error) {
	data, err := readOptional(a.path("workbuddy-restore.json"))
	if err != nil || data == nil {
		return nil, err
	}
	var saved workBuddyRestore
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&saved) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("Invalid saved WorkBuddy configuration")
	}
	if len(saved.Models) == 0 && saved.Managed != nil {
		saved.Models = []workBuddyModelRestore{{Original: saved.Original, Managed: saved.Managed}}
		saved.Original, saved.Managed = nil, nil
	}
	for _, record := range saved.Models {
		id := str(record.Managed["id"])
		if id != workBuddyModelID && !strings.HasPrefix(id, workBuddyModelPrefix) {
			return nil, errors.New("Invalid saved WorkBuddy model ID")
		}
	}
	return &saved, nil
}

func (a *App) injectWorkBuddy() error {
	if err := acquireClientAccess(a.options.Home, "workbuddy"); err != nil {
		return err
	}
	models, err := a.workBuddyCatalog(a.ctx)
	if err != nil {
		return err
	}
	return a.syncWorkBuddyEntries(a.workBuddyEntries(models))
}

func (a *App) syncWorkBuddyEntries(entries []object) error {
	doc, err := a.readWorkBuddyModels()
	if err != nil {
		return err
	}
	saved, err := a.workBuddyJournal()
	if err != nil {
		return err
	}
	if saved == nil {
		saved = &workBuddyRestore{Created: doc.data == nil}
	}
	previousJournal := jsonBytes(saved)
	records := map[string]workBuddyModelRestore{}
	for _, record := range saved.Models {
		records[str(record.Managed["id"])] = record
	}
	if _, upgrading := records[workBuddyModelID]; upgrading && len(entries) > 0 {
		// WorkBuddy's supported model aliases migrate a saved picker selection to
		// an explicitly named model. There is still no virtual model or proxy fallback.
		entries[0]["aliases"] = []any{workBuddyModelID}
	}
	desired := map[string]object{}
	for _, entry := range entries {
		desired[str(entry["id"])] = entry
	}
	seen := map[string]bool{}
	next := []any{}
	for _, item := range doc.models {
		id := str(obj(item)["id"])
		entry, want := desired[id]
		record, managed := records[id]
		if !want && !managed {
			next = append(next, item)
			continue
		}
		if seen[id] {
			return errors.New("Duplicate ModelBridge entries in WorkBuddy models.json")
		}
		seen[id] = true
		if managed && !workBuddyManaged(item, record) {
			next = append(next, item) // An explicit user edit wins, including during later catalog syncs.
			continue
		}
		if !want {
			if record.Original != nil {
				next = append(next, record.Original)
			}
			continue
		}
		if !managed {
			record.Original = obj(item)
		}
		if !bytes.Equal(jsonBytes(record.Managed), jsonBytes(entry)) {
			record.Previous = record.Managed
			record.Managed = entry
		}
		records[id] = record
		next = append(next, entry)
	}
	for _, entry := range entries {
		id := str(entry["id"])
		if seen[id] {
			continue
		}
		record := records[id]
		if !bytes.Equal(jsonBytes(record.Managed), jsonBytes(entry)) {
			record.Previous = record.Managed
			record.Managed = entry
		}
		records[id] = record
		next = append(next, entry)
	}
	saved.Models = nil
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		saved.Models = append(saved.Models, records[id])
	}
	// Keep both old and pending managed values until the file replacement succeeds,
	// so a failed write or process exit cannot lose the original client configuration.
	if !bytes.Equal(previousJournal, jsonBytes(saved)) {
		if err = writeJSON(a.path("workbuddy-restore.json"), saved); err != nil {
			return err
		}
	}
	if bytes.Equal(jsonBytes(doc.models), jsonBytes(next)) {
		return nil
	}
	doc.models = next
	return a.writeWorkBuddyModels(doc)
}

func (a *App) restoreWorkBuddy() error {
	saved, err := a.workBuddyJournal()
	if err != nil || saved == nil {
		return err
	}
	if err = acquireClientAccess(a.options.Home, "workbuddy"); err != nil {
		return err
	}
	doc, err := a.readWorkBuddyModels()
	if err != nil {
		return err
	}
	records := map[string]workBuddyModelRestore{}
	for _, record := range saved.Models {
		records[str(record.Managed["id"])] = record
	}
	next := []any{}
	changed := false
	for _, item := range doc.models {
		record, managed := records[str(obj(item)["id"])]
		if managed && workBuddyManaged(item, record) {
			changed = true
			if record.Original != nil {
				next = append(next, record.Original)
			}
		} else {
			next = append(next, item)
		}
	}
	if changed {
		doc.models = next
		if err = a.writeWorkBuddyModels(doc); err != nil {
			return err
		}
		if saved.Created && len(next) == 0 && doc.root == nil {
			if err = removeOptional(a.workBuddyConfigPath()); err != nil {
				return err
			}
		}
	}
	return removeOptional(a.path("workbuddy-restore.json"))
}

func (a *App) clientStates() object {
	s := a.snapshot()
	return object{"codexEnabled": s.Codex, "claudeEnabled": s.Claude,
		"copilotEnabled": s.Copilot, "workbuddyEnabled": s.WorkBuddy}
}

func (a *App) toggleWorkBuddy() error {
	if !a.routes.TryLock() {
		return errors.New("A request is running. Try again when it finishes")
	}
	defer a.routes.Unlock()
	previous := a.snapshot()
	if !previous.active() {
		return errors.New("not connected")
	}
	next := previous
	next.WorkBuddy = !previous.WorkBuddy
	if next.WorkBuddy {
		if err := a.injectWorkBuddy(); err != nil {
			return errors.Join(err, a.restoreWorkBuddy())
		}
	} else if err := a.restoreWorkBuddy(); err != nil {
		return err
	}
	if err := writeJSON(a.path("session.json"), next); err != nil {
		if next.WorkBuddy {
			return errors.Join(err, a.restoreWorkBuddy())
		}
		return errors.Join(err, a.injectWorkBuddy())
	}
	a.mu.Lock()
	a.state = next
	a.mu.Unlock()
	return nil
}

func (a *App) startWorkBuddyWatcher() {
	a.workBuddyWatchOnce.Do(func() {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			lastError := ""
			for {
				select {
				case <-a.ctx.Done():
					return
				case <-ticker.C:
					if !a.actions.TryLock() {
						continue
					}
					if a.ctx.Err() != nil {
						a.actions.Unlock()
						return
					}
					state := a.snapshot()
					if !state.active() || !state.WorkBuddy {
						a.actions.Unlock()
						continue
					}
					err := a.injectWorkBuddy()
					a.actions.Unlock()
					if err != nil && a.ctx.Err() == nil {
						if err.Error() != lastError {
							a.report(err)
						}
						lastError = err.Error()
					} else {
						lastError = ""
					}
				}
			}
		}()
	})
}

func (a *App) workBuddy(w http.ResponseWriter, r *http.Request) {
	w, r, done := logProxyRequest(w, r, "WorkBuddy")
	defer done()
	if r.Header.Get("Origin") != "" {
		respond(w, 403, object{"error": "Browser origins are not allowed"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer PROXY_MANAGED")) != 1 {
		respond(w, 401, object{"error": "Set the WorkBuddy API key to PROXY_MANAGED"})
		return
	}
	if !((r.Method == "GET" && r.URL.Path == "/v1/models") || (r.Method == "POST" && r.URL.Path == "/v1/chat/completions")) {
		respond(w, 404, object{"error": "Inference endpoint not found"})
		return
	}
	a.openAIFor(w, r, true, true)
}

func (a *App) prepareWorkBuddyRequest(w http.ResponseWriter, r *http.Request, s session) bool {
	models, err := a.workBuddyCatalog(r.Context())
	if err != nil {
		respond(w, 502, object{"error": err.Error()})
		return false
	}
	if r.Method == "GET" {
		list := []any{}
		for _, entry := range a.workBuddyEntries(models) {
			list = append(list, object{"id": entry["id"], "name": entry["name"], "object": "model", "owned_by": "modelbridge"})
		}
		respond(w, 200, object{"object": "list", "data": list})
		return false
	}
	body, err := readObject(r.Body, 256<<20)
	if err != nil {
		respond(w, 400, object{"error": "Invalid WorkBuddy request"})
		return false
	}
	requested := str(body["model"])
	var chosen object
	if requested == workBuddyModelID {
		respond(w, 400, object{"error": "ModelBridge is a provider. Select a specific model in WorkBuddy"})
		return false
	}
	if strings.HasPrefix(requested, workBuddyModelPrefix) {
		id := strings.TrimPrefix(requested, workBuddyModelPrefix)
		for _, model := range models {
			if model["id"] == id {
				chosen = model
				break
			}
		}
	}
	if chosen == nil {
		respond(w, 400, object{"error": "This WorkBuddy model is unavailable for the selected ModelBridge account; choose a current model"})
		return false
	}
	body["model"] = chosen["id"]
	if s.LocalModel == "" && !contains(stringsOf(chosen["supported_endpoints"]), "/chat/completions") {
		a.workBuddyResponses(w, r, body, requested)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(jsonBytes(body)))
	return true
}
