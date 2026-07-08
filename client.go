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
	// SetCurrent, when non-nil, tells the server whether this snapshot should
	// become the "current" save for its slot. Omitted (nil) keeps the default
	// server behavior (it becomes current). It is set to false only when
	// uploading local bytes as a conflict backup before pulling the server copy.
	SetCurrent *bool `json:"set_current,omitempty"`
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

// PullDisabledError signals that the server has turned off the pull endpoint
// (HTTP 503). The agent degrades gracefully instead of treating it as a crash.
type PullDisabledError struct {
	Message string
}

func (e *PullDisabledError) Error() string { return e.Message }

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

// setCommonHeaders applies the auth, user-agent and locale headers shared by
// every request. The agent is a machine client; its logs are diagnostic and
// must be canonical English, independent of the account's web UI locale. The
// server honors Accept-Language over the token owner's locale.
func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "grailward-agent/"+Version)
	req.Header.Set("Accept-Language", "en")
}

// authGet builds a GET request against the API base with the common headers.
// path may be an absolute path already URL-encoded by the server (e.g. a
// download_path); it is used verbatim over the base URL's scheme and host.
func (c *Client) authGet(path string) (*http.Request, error) {
	base, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL: %w", err)
	}
	req, err := http.NewRequest("GET", base.Scheme+"://"+base.Host+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	c.setCommonHeaders(req)
	return req, nil
}

// apiErrorMessage renders a non-2xx response body into a short diagnostic line,
// preferring the JSON {error} field and falling back to a body snippet.
func apiErrorMessage(status int, body []byte) string {
	if msg := errorText(body); msg != "" {
		return fmt.Sprintf("HTTP %d - %s", status, msg)
	}
	snippet := string(body)
	if len(snippet) > 120 {
		snippet = snippet[:120]
	}
	return fmt.Sprintf("HTTP %d - %s", status, snippet)
}

// errorText extracts the JSON {error} field from a response body, or "".
func errorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		return e.Error
	}
	return ""
}

// UploadSnapshot sends the file snapshot to the Grailward API. The snapshot
// becomes the current save for its slot (default server behavior).
func (c *Client) UploadSnapshot(filename string, fileBytes []byte, sha256Hex string, machine string) (*SnapshotResponse, error) {
	return c.uploadSnapshot(filename, fileBytes, sha256Hex, machine, nil)
}

// UploadSnapshotBackup sends local bytes as a non-current snapshot. It is used
// on a two-way conflict when the user chooses the server copy: the local bytes
// are guaranteed into the server's history before any overwrite, without
// becoming the current save.
func (c *Client) UploadSnapshotBackup(filename string, fileBytes []byte, sha256Hex string, machine string) (*SnapshotResponse, error) {
	setCurrent := false
	return c.uploadSnapshot(filename, fileBytes, sha256Hex, machine, &setCurrent)
}

func (c *Client) uploadSnapshot(filename string, fileBytes []byte, sha256Hex string, machine string, setCurrent *bool) (*SnapshotResponse, error) {
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
		SetCurrent:    setCurrent,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", reqURL.String(), bytes.NewBuffer(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	c.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

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

	snapResp.Error = apiErrorMessage(resp.StatusCode, respBytes)
	return &snapResp, nil
}

// FetchManifest retrieves the server's current sync manifest (characters and
// shared stashes). A 503 degrades to a PullDisabledError so callers can back
// off without treating it as a hard failure.
func (c *Client) FetchManifest() (*Manifest, error) {
	req, err := c.authGet("/api/v1/sync")
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest body: %w", err)
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		msg := errorText(body)
		if msg == "" {
			msg = "server pull is temporarily unavailable"
		}
		return nil, &PullDisabledError{Message: msg}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("manifest fetch failed: %s", apiErrorMessage(resp.StatusCode, body))
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("failed to parse manifest JSON: %w", err)
	}
	return &m, nil
}

// Download fetches the raw bytes at a server-provided download_path (used
// verbatim) and returns them together with the server's X-Sha256 header. The
// bytes are NOT verified here; the caller checks the sha against both this
// header and the manifest before writing anything.
func (c *Client) Download(downloadPath string) ([]byte, string, error) {
	req, err := c.authGet(downloadPath)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read download body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("download failed: %s", apiErrorMessage(resp.StatusCode, body))
	}
	return body, resp.Header.Get("X-Sha256"), nil
}
