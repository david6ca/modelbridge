package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WorkBuddy speaks Chat Completions, including for Responses-only Copilot models.
// Translate at this boundary; never replace the user's selected model with another.
func workBuddyResponseRequest(chat object) (object, error) {
	input := []any{}
	for _, value := range arr(chat["messages"]) {
		message := obj(value)
		role := str(message["role"])
		if role == "tool" {
			if str(message["tool_call_id"]) == "" {
				return nil, errors.New("WorkBuddy tool result has no tool_call_id")
			}
			output := message["content"]
			if output == nil {
				output = ""
			}
			input = append(input, object{"type": "function_call_output", "call_id": message["tool_call_id"], "output": output})
			continue
		}
		if !contains([]string{"system", "developer", "user", "assistant"}, role) {
			return nil, errors.New("Unsupported WorkBuddy message role")
		}
		if message["content"] != nil {
			content := message["content"]
			if parts, ok := content.([]any); ok {
				converted := []any{}
				for _, part := range parts {
					p := obj(part)
					switch str(p["type"]) {
					case "text":
						kind := "input_text"
						if role == "assistant" {
							kind = "output_text"
						}
						converted = append(converted, object{"type": kind, "text": p["text"]})
					case "image_url":
						image := obj(p["image_url"])
						entry := object{"type": "input_image", "image_url": image["url"]}
						if image["detail"] != nil {
							entry["detail"] = image["detail"]
						}
						converted = append(converted, entry)
					default:
						return nil, errors.New("Unsupported WorkBuddy message content type")
					}
				}
				content = converted
			}
			input = append(input, object{"role": role, "content": content})
		}
		for _, call := range arr(message["tool_calls"]) {
			c := obj(call)
			f := obj(c["function"])
			if str(c["id"]) == "" || str(f["name"]) == "" || str(c["type"]) != "function" {
				return nil, errors.New("Invalid WorkBuddy function call history")
			}
			input = append(input, object{"type": "function_call", "call_id": c["id"], "name": f["name"], "arguments": f["arguments"]})
		}
	}
	out := object{"model": chat["model"], "input": input, "stream": chat["stream"] == true}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if chat[key] != nil {
			out[key] = chat[key]
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if chat[key] != nil {
			out["max_output_tokens"] = chat[key]
		}
	}
	if effort := str(chat["reasoning_effort"]); effort != "" {
		out["reasoning"] = object{"effort": effort}
	}
	if tools := arr(chat["tools"]); len(tools) > 0 {
		converted := []any{}
		for _, tool := range tools {
			t := obj(tool)
			if t["type"] != "function" || str(nested(t, "function", "name")) == "" {
				return nil, errors.New("WorkBuddy requires function tools")
			}
			entry := clone(obj(t["function"]))
			entry["type"] = "function"
			converted = append(converted, entry)
		}
		out["tools"] = converted
	}
	if choice := chat["tool_choice"]; choice != nil {
		if named := obj(choice); named != nil {
			out["tool_choice"] = object{"type": "function", "name": nested(named, "function", "name")}
		} else {
			out["tool_choice"] = choice
		}
	}
	if format := obj(chat["response_format"]); format != nil {
		converted := clone(format)
		if format["type"] == "json_schema" {
			converted = clone(obj(format["json_schema"]))
			if converted == nil {
				return nil, errors.New("Invalid WorkBuddy JSON schema")
			}
			converted["type"] = "json_schema"
		}
		out["text"] = object{"format": converted}
	}
	if number(chat["n"]) > 1 {
		return nil, errors.New("WorkBuddy Responses models support one completion per request")
	}
	return out, nil
}

func workBuddyUsage(response object) object {
	u := obj(response["usage"])
	if u == nil {
		return nil
	}
	out := object{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"]}
	if u["input_tokens_details"] != nil {
		out["prompt_tokens_details"] = u["input_tokens_details"]
	}
	if u["output_tokens_details"] != nil {
		out["completion_tokens_details"] = u["output_tokens_details"]
	}
	return out
}

func workBuddyFinish(response object, tools bool) string {
	if response["status"] == "incomplete" {
		if nested(response, "incomplete_details", "reason") == "content_filter" {
			return "content_filter"
		}
		return "length"
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

func workBuddyChatResponse(response object, model string) object {
	if response["error"] != nil || response["status"] == "failed" {
		return object{"error": response["error"]}
	}
	text, refusal := "", ""
	calls := []any{}
	for _, value := range arr(response["output"]) {
		item := obj(value)
		if item["type"] == "function_call" {
			calls = append(calls, object{"id": item["call_id"], "type": "function", "function": object{"name": item["name"], "arguments": item["arguments"]}})
		}
		for _, part := range arr(item["content"]) {
			p := obj(part)
			if p["type"] == "output_text" {
				text += str(p["text"])
			}
			if p["type"] == "refusal" {
				refusal += str(p["refusal"])
			}
		}
	}
	message := object{"role": "assistant", "content": text}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	return object{"id": response["id"], "object": "chat.completion", "created": response["created_at"], "model": model,
		"choices": []any{object{"index": 0, "message": message, "finish_reason": workBuddyFinish(response, len(calls) > 0)}}, "usage": workBuddyUsage(response)}
}

func (a *App) workBuddyResponses(w http.ResponseWriter, r *http.Request, chat object, model string) {
	body, err := workBuddyResponseRequest(chat)
	if err != nil {
		respond(w, 400, object{"error": err.Error()})
		return
	}
	authRetried := false
	for attempt := 0; attempt < 4; attempt++ {
		upstream, err := a.copilotWithRetry(r.Context(), http.MethodPost, "/v1/responses", jsonBytes(body), &authRetried)
		if err != nil {
			respond(w, 502, object{"error": err.Error()})
			return
		}
		if upstream.StatusCode != 200 {
			data, err := readResponse(upstream, 16<<20)
			if err != nil {
				respond(w, 502, object{"error": err.Error()})
				return
			}
			if upstream.StatusCode == 400 && attempt < 3 {
				var problem object
				if json.Unmarshal(data, &problem) == nil {
					if next, _, changed := repairRequest(body, problem); changed {
						body = next
						continue
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(upstream.StatusCode)
			_, _ = w.Write(data)
			return
		}
		if chat["stream"] == true {
			defer upstream.Body.Close()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			if err := workBuddyChatStream(upstream.Body, w, model); err != nil && r.Context().Err() == nil {
				_, _ = io.WriteString(w, "data: "+string(jsonBytes(object{"error": object{"code": "upstream_stream_interrupted", "message": err.Error()}}))+"\n\n")
				_ = http.NewResponseController(w).Flush()
			}
			return
		}
		data, err := readResponse(upstream, 16<<20)
		var response object
		if err == nil {
			err = json.Unmarshal(data, &response)
		}
		if err != nil || response == nil {
			respond(w, 502, object{"error": "Invalid WorkBuddy upstream response"})
			return
		}
		status := 200
		if response["error"] != nil || response["status"] == "failed" {
			status = 502
		}
		respond(w, status, workBuddyChatResponse(response, model))
		return
	}
}

type workBuddyStreamCall struct {
	index               int
	id, name, arguments string
	announced           bool
}

func workBuddyChatStream(source io.Reader, w http.ResponseWriter, model string) error {
	terminal := errors.New("WorkBuddy response complete")
	id, created := "chatcmpl-modelbridge", time.Now().Unix()
	calls := map[int]*workBuddyStreamCall{}
	texts := map[int]string{}
	roleSent := false
	emit := func(delta object, finish any, usage object) error {
		if !roleSent {
			delta["role"] = "assistant"
			roleSent = true
		}
		chunk := object{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{object{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			chunk["usage"] = usage
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", jsonBytes(chunk)); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	tool := func(index int, item object, arguments string, complete bool) error {
		call := calls[index]
		if call == nil {
			call = &workBuddyStreamCall{index: len(calls)}
			calls[index] = call
		}
		if str(item["call_id"]) != "" {
			call.id = str(item["call_id"])
		}
		if str(item["name"]) != "" {
			call.name = str(item["name"])
		}
		if complete {
			if !strings.HasPrefix(arguments, call.arguments) {
				return errors.New("WorkBuddy tool arguments changed after streaming")
			}
			arguments = strings.TrimPrefix(arguments, call.arguments)
		}
		if !call.announced && (call.id == "" || call.name == "") {
			return errors.New("WorkBuddy upstream tool call is missing its identity")
		}
		function := object{"arguments": arguments}
		entry := object{"index": call.index, "function": function}
		if !call.announced {
			entry["id"], entry["type"], function["name"] = call.id, "function", call.name
			call.announced = true
		}
		call.arguments += arguments
		return emit(object{"tool_calls": []any{entry}}, nil, nil)
	}
	err := readSSE(source, func(e sseEvent) error {
		event, _ := e.data()
		if event == nil {
			return nil
		}
		index := int(number(event["output_index"]))
		item := obj(event["item"])
		switch str(event["type"]) {
		case "response.created":
			if response := obj(event["response"]); response != nil {
				if str(response["id"]) != "" {
					id = str(response["id"])
				}
			}
		case "response.output_text.delta":
			delta := str(event["delta"])
			texts[index] += delta
			return emit(object{"content": delta}, nil, nil)
		case "response.refusal.delta":
			return emit(object{"refusal": event["delta"]}, nil, nil)
		case "response.reasoning_summary_text.delta":
			return emit(object{"reasoning_content": event["delta"]}, nil, nil)
		case "response.output_item.added":
			if item["type"] == "function_call" {
				return tool(index, item, str(item["arguments"]), false)
			}
		case "response.function_call_arguments.delta":
			return tool(index, object{}, str(event["delta"]), false)
		case "response.output_item.done":
			if item["type"] == "function_call" {
				return tool(index, item, str(item["arguments"]), true)
			}
			for _, part := range arr(item["content"]) {
				p := obj(part)
				if p["type"] == "output_text" && texts[index] == "" {
					texts[index] = str(p["text"])
					if err := emit(object{"content": p["text"]}, nil, nil); err != nil {
						return err
					}
				}
			}
		case "response.completed", "response.incomplete":
			response := obj(event["response"])
			if err := emit(object{}, workBuddyFinish(response, len(calls) > 0), workBuddyUsage(response)); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
				return err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			return terminal
		case "response.failed", "error":
			problem := obj(event["response"])
			if problem == nil {
				problem = event
			}
			detail := parseError(problem)
			if detail != nil && str(detail["message"]) != "" {
				return errors.New(str(detail["message"]))
			}
			return errors.New("WorkBuddy upstream inference failed")
		}
		return nil
	})
	if errors.Is(err, terminal) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("WorkBuddy upstream stream ended before completion")
}
