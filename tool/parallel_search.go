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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThinkInAIXYZ/go-mcp/client"
	"github.com/ThinkInAIXYZ/go-mcp/protocol"
	"github.com/ThinkInAIXYZ/go-mcp/transport"
	"github.com/the-open-agent/openagent/internal/cli"
)

const parallelSearchEndpoint = "https://search.parallel.ai/mcp"

// The shared mcp package imports tool, so this adapter uses the same SDK directly.
func runParallelSearch(ctx context.Context, params webSearchParams, baseClient *http.Client) ([]webSearchResult, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, webSearchTimeout)
	defer cancel()

	httpClient := *baseClient
	httpClient.Transport = &parallelSearchTransport{ctx: ctx, base: baseClient.Transport}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Parallel search redirects are not supported")
	}
	tr, err := transport.NewStreamableHTTPClientTransport(parallelSearchEndpoint,
		transport.WithStreamableHTTPClientOptionHTTPClient(&httpClient),
	)
	if err != nil {
		return nil, nil, err
	}
	mcpClient, err := client.NewClient(tr,
		client.WithInitTimeout(webSearchTimeout),
		client.WithClientInfo(&protocol.Implementation{Name: "openagent", Version: cli.Version}),
	)
	if err != nil {
		_ = tr.Close()
		return nil, nil, err
	}
	defer mcpClient.Close()

	tools, err := mcpClient.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	available := false
	for _, tool := range tools.Tools {
		if tool.Name == "web_search" {
			available = true
			break
		}
	}
	if !available {
		return nil, nil, fmt.Errorf("Parallel MCP server does not expose web_search")
	}
	// The builtin interface has no conversation identity. Omit optional session
	// metadata rather than introduce shared state across users or requests.
	response, err := mcpClient.CallTool(ctx, &protocol.CallToolRequest{
		Name: "web_search",
		Arguments: map[string]interface{}{
			"objective":      params.Query,
			"search_queries": []string{params.Query},
		},
	})
	if err != nil {
		return nil, nil, err
	}
	if response.IsError {
		message := "Parallel MCP tool returned an error"
		for _, content := range response.Content {
			if text, ok := content.(*protocol.TextContent); ok && text.Type == "text" {
				message += ": " + text.Text
				break
			}
		}
		return nil, nil, fmt.Errorf("%s", message)
	}

	// This SDK version exposes text content, not structuredContent. Decode only
	// the text representation so duplicated server content is never added twice.
	for _, content := range response.Content {
		text, ok := content.(*protocol.TextContent)
		if !ok || text.Type != "text" {
			continue
		}
		var payload struct {
			Results *[]struct {
				URL      string   `json:"url"`
				Title    string   `json:"title"`
				Excerpts []string `json:"excerpts"`
			} `json:"results"`
			Warnings []string `json:"warnings"`
		}
		if err := json.Unmarshal([]byte(text.Text), &payload); err != nil {
			return nil, nil, fmt.Errorf("invalid Parallel search payload: %w", err)
		}
		if payload.Results == nil {
			return nil, nil, fmt.Errorf("Parallel search payload is missing a results array")
		}
		results := make([]webSearchResult, 0, len(*payload.Results))
		for _, item := range *payload.Results {
			parsedURL, err := url.Parse(item.URL)
			if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Hostname() == "" {
				return nil, nil, fmt.Errorf("Parallel search returned an invalid result URL")
			}
			title := item.Title
			if strings.TrimSpace(title) == "" {
				title = item.URL
			}
			results = append(results, webSearchResult{
				Title:    cleanWebSearchText(title),
				URL:      item.URL,
				Snippet:  strings.Join(item.Excerpts, "\n\n"),
				SiteName: parsedURL.Hostname(),
			})
		}
		for i := range payload.Warnings {
			payload.Warnings[i] = wrapWebSearchContent(payload.Warnings[i])
		}
		return limitWebSearchResults(results, params.Count), payload.Warnings, nil
	}
	return nil, nil, fmt.Errorf("Parallel MCP response is missing text search content")
}

// The SDK uses its own background context for HTTP requests. Merge it with
// the caller's deadline here, and bound reads before the SDK buffers a body.
type parallelSearchTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t *parallelSearchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(t.ctx)
	stop := context.AfterFunc(req.Context(), cancel)
	finish := func() {
		stop()
		cancel()
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	request := req.Clone(ctx)
	// Identify the project for aggregate free MCP usage, without user identifiers.
	// Apply it to discovery, calls, and transport cleanup alike.
	request.Header.Set("User-Agent", "openagent/"+cli.Version)
	response, err := base.RoundTrip(request)
	if err != nil {
		finish()
		return nil, err
	}
	response.Body = &parallelSearchBody{ReadCloser: response.Body, remaining: webSearchMaxResponseSize, finish: finish}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		// Some edge errors have a null response ID. The SDK cannot correlate
		// them, so preserve the service failure instead of waiting for a timeout.
		var envelope struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil && (len(envelope.ID) == 0 || string(envelope.ID) == "null") {
			return nil, fmt.Errorf("Parallel MCP error: %s", envelope.Error.Message)
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	return response, nil
}

type parallelSearchBody struct {
	io.ReadCloser
	remaining int64
	finish    func()
}

func (b *parallelSearchBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, fmt.Errorf("Parallel response body exceeds %d bytes", webSearchMaxResponseSize)
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, fmt.Errorf("Parallel response body exceeds %d bytes", webSearchMaxResponseSize)
	}
	return n, err
}

func (b *parallelSearchBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}
