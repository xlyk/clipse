package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// apiKeyEnvVar is the environment variable the transport reads its Linear
// API key from.
const apiKeyEnvVar = "LINEAR_API_KEY"

// graphqlResponse is the wire shape of a GraphQL response: a "data" payload
// alongside an optional "errors" list, per the GraphQL spec.
type graphqlResponse struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// transport is the shared HTTP+GraphQL plumbing for talking to Linear's API:
// it holds the API key, endpoint, and http.Client, and POSTs prebuilt request
// bodies. Both the dispatcher's HTTPClient and the board-bootstrap client
// embed it, so neither duplicates auth/envelope handling — and extracting it
// does not widen HTTPClient's method surface (no issue-create/delete leaks
// onto the dispatcher client).
type transport struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// newTransport builds a transport pointed at baseURL, reading the API key from
// LINEAR_API_KEY. Returns an error if the variable is unset or empty.
func newTransport(baseURL string) (*transport, error) {
	apiKey := os.Getenv(apiKeyEnvVar)
	if apiKey == "" {
		return nil, fmt.Errorf("%s is not set", apiKeyEnvVar)
	}
	return &transport{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// do POSTs a prebuilt GraphQL request body to Linear's API and returns the
// raw response body, after checking the HTTP status and any GraphQL-level
// "errors" array.
func (t *transport) do(ctx context.Context, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", t.apiKey)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linear api returned status %d: %s", resp.StatusCode, respBody)
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBody, &gqlResp); err != nil {
		return nil, fmt.Errorf("decoding response envelope: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("linear api returned errors: %s", gqlResp.Errors[0].Message)
	}

	return respBody, nil
}
