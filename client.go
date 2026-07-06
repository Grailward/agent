package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type SnapshotRequest struct {
	RawBase64     string `json:"raw_base64"`
	SHA256        string `json:"sha256"`
	SourceMachine string `json:"source_machine"`
	Filename      string `json:"filename"`
}

type SnapshotResponse struct {
	Status      string `json:"status"`
	SharedStash *struct {
		Mode      string `json:"mode"`
		ItemCount int    `json:"item_count"`
	} `json:"shared_stash"`
	Character *struct {
		Name  string `json:"name"`
		Level int    `json:"level"`
	} `json:"character"`
	Error string `json:"error"`
}

type Client struct {
	URL        string
	Token      string
	HTTPClient *http.Client
}

func NewClient(apiURL, token string) *Client {
	return &Client{
		URL:   apiURL,
		Token: token,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// SetToken swaps the bearer token used for subsequent uploads (tray reset).
func (c *Client) SetToken(token string) {
	c.Token = token
}

// UploadSnapshot sends the file snapshot to the Grailward API.
func (c *Client) UploadSnapshot(filename string, fileBytes []byte, sha256Hex string, machine string) (*SnapshotResponse, error) {
	reqURL, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL: %w", err)
	}
	reqURL.Path = "/api/v1/snapshots"

	reqBody := SnapshotRequest{
		RawBase64:     base64.StdEncoding.EncodeToString(fileBytes),
		SHA256:        sha256Hex,
		SourceMachine: machine,
		Filename:      filename,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", reqURL.String(), bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grailward-agent/"+Version)
	// The agent is a machine client; its logs are diagnostic and must be
	// canonical English, independent of the account's web UI locale. The
	// server honors this over the token owner's locale.
	req.Header.Set("Accept-Language", "en")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var snapResp SnapshotResponse
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(respBytes, &snapResp); err != nil {
			return nil, fmt.Errorf("failed to parse response JSON: %w", err)
		}
		return &snapResp, nil
	}

	// Try parsing JSON error format first, fallback to snippet
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &errBody); err == nil && errBody.Error != "" {
		snapResp.Error = fmt.Sprintf("HTTP %d - %s", resp.StatusCode, errBody.Error)
	} else {
		bodySnippet := string(respBytes)
		if len(bodySnippet) > 120 {
			bodySnippet = bodySnippet[:120]
		}
		snapResp.Error = fmt.Sprintf("HTTP %d - %s", resp.StatusCode, bodySnippet)
	}
	return &snapResp, nil
}
