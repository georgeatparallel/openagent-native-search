// Copyright 2026 The OpenAgent Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThinkInAIXYZ/go-mcp/protocol"
	"github.com/the-open-agent/openagent/internal/cli"
	"github.com/the-open-agent/openagent/proxy"
)

type parallelTestRoundTripper func(*http.Request) (*http.Response, error)

func (f parallelTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type parallelTestRequest struct {
	ID     interface{}            `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}

func parallelTestServer(t *testing.T, call func(http.ResponseWriter, *http.Request, parallelTestRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req parallelTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode native request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			parallelTestReply(w, req.ID, map[string]interface{}{
				"protocolVersion": "2025-03-26", "capabilities": map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo": map[string]interface{}{"name": "fixture", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			parallelTestReply(w, req.ID, map[string]interface{}{"tools": []interface{}{map[string]interface{}{
				"name": "web_search", "description": "Search", "inputSchema": map[string]interface{}{"type": "object"},
			}}})
		default:
			call(w, r, req)
		}
	}))
}

func parallelTestReply(w http.ResponseWriter, id interface{}, result interface{}) {
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
}

func parallelTestContent(payload string) map[string]interface{} {
	return map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "text", "text": payload}}}
}

func parallelTestRoute(t *testing.T, server *httptest.Server, capture func(*http.Request)) {
	t.Helper()
	original := webSearchHTTPClient
	t.Cleanup(func() { webSearchHTTPClient = original })
	endpoint, _ := url.Parse(server.URL)
	webSearchHTTPClient = &http.Client{Transport: parallelTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if capture != nil {
			capture(r)
		}
		request := r.Clone(r.Context())
		request.URL.Scheme = endpoint.Scheme
		request.URL.Host = endpoint.Host
		return http.DefaultTransport.RoundTrip(request)
	})}
}

func parallelTestNative(t *testing.T) BuiltinTool {
	t.Helper()
	configured, err := New(Config{Type: "web_search", SubType: "Parallel", ProviderUrl: "https://unused.example", ClientSecret: "unused-saved-secret"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	tools := configured.BuiltinTools()
	if len(tools) != 1 || tools[0].GetName() != "web_search" {
		t.Fatalf("Parallel must expose only canonical text search: %v", tools)
	}
	return tools[0]
}

func parallelTestExecute(t *testing.T, native BuiltinTool, args map[string]interface{}) *protocol.CallToolResult {
	t.Helper()
	result, err := native.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestParallelNativeSearchWire(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	var queries []string
	server := parallelTestServer(t, func(w http.ResponseWriter, r *http.Request, req parallelTestRequest) {
		if req.Method != "tools/call" || req.Params["name"] != "web_search" {
			t.Errorf("unexpected canonical call: %+v", req)
		}
		args := req.Params["arguments"].(map[string]interface{})
		query := args["objective"].(string)
		if len(args) != 2 || args["search_queries"].([]interface{})[0] != query {
			t.Errorf("input mapping: %v", args)
		}
		queries = append(queries, query)
		parallelTestReply(w, req.ID, parallelTestContent(`{"results":[{"url":"https://example.com/one","title":"First","excerpts":["one","two"]},{"url":"https://example.com/two","title":null,"excerpts":[]}],"warnings":["Query adjusted"]}`))
	})
	defer server.Close()
	parallelTestRoute(t, server, func(r *http.Request) {
		if r.URL.String() != parallelSearchEndpoint || r.Header.Get("User-Agent") != "openagent/"+cli.Version || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("outbound endpoint/attribution/auth: %s %v", r.URL, r.Header)
		}
		if r.Method == http.MethodPost {
			if !strings.Contains(r.Header.Get("Accept"), "application/json") || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				t.Errorf("MCP Accept: %s", r.Header.Get("Accept"))
			}
			var request parallelTestRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			r.Body, _ = r.GetBody()
			mu.Lock()
			methods = append(methods, request.Method)
			mu.Unlock()
		}
	})
	native := parallelTestNative(t)
	properties := native.GetInputSchema().(map[string]interface{})["properties"].(map[string]interface{})
	if properties["language"] != nil || properties["country"] != nil {
		t.Fatal("Parallel schema advertises unsupported controls")
	}
	for _, query := range []string{"first query", "second query"} {
		result := parallelTestExecute(t, native, map[string]interface{}{"query": query, "count": 1})
		if result.IsError {
			t.Fatal(result.Content)
		}
		var payload webSearchPayload
		if err := json.Unmarshal([]byte(result.Content[0].(*protocol.TextContent).Text), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Provider != "parallel" || payload.Query != query || payload.Count != 1 || !payload.ExternalContent.Untrusted || len(payload.Results) != 1 || payload.Results[0].URL != "https://example.com/one" || payload.Results[0].SiteName != "example.com" || !strings.Contains(payload.Results[0].Snippet, "one\n\ntwo") || !strings.Contains(payload.Results[0].Title, "[Untrusted web_search content]") || len(payload.Warnings) != 1 {
			t.Fatalf("caller-visible evidence lost: %+v", payload)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/list,tools/call,initialize,notifications/initialized,tools/list,tools/call" || len(queries) != 2 {
		t.Fatalf("native discovery/call/reconnect journey: %v / %v", methods, queries)
	}
}

func TestParallelUnsupportedControlsMakeNoRequest(t *testing.T) {
	original := webSearchHTTPClient
	defer func() { webSearchHTTPClient = original }()
	webSearchHTTPClient = &http.Client{Transport: parallelTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("unsupported controls must fail before transmitting")
		return nil, fmt.Errorf("unexpected request")
	})}
	for _, key := range []string{"language", "country"} {
		result := parallelTestExecute(t, parallelTestNative(t), map[string]interface{}{"query": "query", key: "en"})
		if !result.IsError || !strings.Contains(result.Content[0].(*protocol.TextContent).Text, key) {
			t.Fatalf("unsupported %s not rejected: %v", key, result)
		}
	}
}

func TestParallelNativeFailuresAndEmpty(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		isError bool
	}{
		{"valid empty", `{"results":[]}`, 200, false},
		{"missing results", `{}`, 200, true},
		{"null results", `{"results":null}`, 200, true},
		{"invalid JSON", `{`, 200, true},
		{"wrong results shape", `{"results":{}}`, 200, true},
		{"invalid URL", `{"results":[{"url":"file:///tmp/private"}]}`, 200, true},
		{"HTTP quota", `rate limited`, 429, true},
		{"tool error", `{"results":[]}`, 200, true},
		{"RPC error", ``, 200, true},
		{"null ID RPC error", ``, 200, true},
		{"oversized", strings.Repeat("x", webSearchMaxResponseSize+1), 200, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := parallelTestServer(t, func(w http.ResponseWriter, r *http.Request, req parallelTestRequest) {
				if test.status != 200 {
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
					return
				}
				if strings.Contains(test.name, "RPC error") {
					id := req.ID
					if test.name == "null ID RPC error" {
						id = nil
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "error": map[string]interface{}{"code": -32000, "message": "quota failed"}})
					return
				}
				result := parallelTestContent(test.body)
				if test.name == "tool error" {
					result["isError"] = true
				}
				parallelTestReply(w, req.ID, result)
			})
			defer server.Close()
			parallelTestRoute(t, server, nil)
			result := parallelTestExecute(t, parallelTestNative(t), map[string]interface{}{"query": "query"})
			if result.IsError != test.isError {
				t.Fatalf("error/empty distinction: %+v", result)
			}
			if !result.IsError && !strings.Contains(result.Content[0].(*protocol.TextContent).Text, `"results":[]`) {
				t.Fatal("valid empty array was lost")
			}
		})
	}
}

func TestParallelCallerDeadlineStopsNativeHTTP(t *testing.T) {
	for _, phase := range []string{"initialize", "tools/call"} {
		t.Run(phase, func(t *testing.T) {
			original := webSearchHTTPClient
			defer func() { webSearchHTTPClient = original }()
			var stopped atomic.Bool
			webSearchHTTPClient = &http.Client{Transport: parallelTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				var req parallelTestRequest
				if r.Method == http.MethodPost {
					_ = json.NewDecoder(r.Body).Decode(&req)
				}
				if req.Method == phase {
					<-r.Context().Done()
					stopped.Store(true)
					return nil, r.Context().Err()
				}
				result := `{"jsonrpc":"2.0","id":` + fmt.Sprint(req.ID) + `,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}}`
				if req.Method == "tools/list" {
					result = `{"jsonrpc":"2.0","id":` + fmt.Sprint(req.ID) + `,"result":{"tools":[{"name":"web_search","inputSchema":{"type":"object"}}]}}`
				}
				status := http.StatusOK
				if req.Method == "notifications/initialized" {
					status = http.StatusAccepted
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(result))}, nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			start := time.Now()
			result, err := parallelTestNative(t).Execute(ctx, map[string]interface{}{"query": "query"})
			if err != nil || !result.IsError || !stopped.Load() || time.Since(start) > time.Second {
				t.Fatalf("caller deadline did not bound %s: %v %v", phase, result, err)
			}
		})
	}
}

func TestParallelRedirectDoesNotReachDestination(t *testing.T) {
	var visited atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { visited.Store(true) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	parallelTestRoute(t, server, nil)
	result := parallelTestExecute(t, parallelTestNative(t), map[string]interface{}{"query": "query"})
	if !result.IsError || visited.Load() {
		t.Fatal("redirect leaked a request to another origin")
	}
}

func TestWebSearchDefaultAndIncumbentsPreserved(t *testing.T) {
	for _, subtype := range []string{"", "DuckDuckGo", "Bing", "Google", "Baidu"} {
		configured, err := New(Config{Type: "web_search", SubType: subtype}, "en")
		if err != nil || len(configured.BuiltinTools()) != 2 {
			t.Fatalf("incumbent tools changed for %s", subtype)
		}
	}
	var outbound atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outbound.Add(1)
		if r.URL.Path != "/html" || !strings.HasPrefix(r.Header.Get("User-Agent"), "Mozilla/") {
			t.Errorf("default route/header changed: %s %v", r.URL, r.Header)
		}
		_, _ = w.Write([]byte(`<div class="result"><a class="result__a" href="https://example.com">Default result</a><span class="result__snippet">Default snippet</span></div>`))
	}))
	defer server.Close()
	parallelTestRoute(t, server, func(r *http.Request) {
		if r.URL.Host != "html.duckduckgo.com" {
			t.Error("default made a new Parallel request")
		}
	})
	configured, err := New(Config{Type: "web_search"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	result := parallelTestExecute(t, configured.BuiltinTools()[0], map[string]interface{}{"query": "query"})
	if result.IsError || outbound.Load() != 1 || !strings.Contains(result.Content[0].(*protocol.TextContent).Text, `"provider":"duckduckgo"`) {
		t.Fatalf("default journey failed: %+v", result)
	}
}

func TestParallelUsesConfiguredProxyTransport(t *testing.T) {
	original := proxy.ProxyHttpClient
	defer func() { proxy.ProxyHttpClient = original }()
	var dispatched atomic.Bool
	proxy.ProxyHttpClient = &http.Client{Transport: parallelTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		dispatched.Store(true)
		if r.URL.String() != parallelSearchEndpoint || r.Header.Get("User-Agent") != "openagent/"+cli.Version {
			t.Error("proxy transport lost endpoint or project attribution")
		}
		return nil, fmt.Errorf("fixture proxy failure")
	})}
	configured, err := New(Config{Type: "web_search", SubType: "Parallel", EnableProxy: true}, "en")
	if err != nil {
		t.Fatal(err)
	}
	result := parallelTestExecute(t, configured.BuiltinTools()[0], map[string]interface{}{"query": "query"})
	if !result.IsError || !dispatched.Load() {
		t.Fatal("Parallel did not use the configured proxy transport")
	}
}
