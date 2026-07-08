package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchManifestParsesAndSetsHeaders(t *testing.T) {
	var gotAuth, gotLang, gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotLang = r.Header.Get("Accept-Language")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"characters": [{"name":"Kristan","filename":"Kristan.d2s","sha256":"aa","size":10,"download_path":"/api/v1/characters/Kristan/current"}],
			"shared_stashes": [{"mode":"softcore","filename":"SharedStashSoftCoreV2.d2i","sha256":"bb","size":20,"download_path":"/api/v1/stashes/softcore/current"}]
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok-123")
	m, err := c.FetchManifest()
	if err != nil {
		t.Fatalf("FetchManifest failed: %v", err)
	}
	if gotPath != "/api/v1/sync" {
		t.Fatalf("manifest path = %q, want /api/v1/sync", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotLang != "en" {
		t.Fatalf("Accept-Language = %q, want en", gotLang)
	}
	if gotUA != "grailward-agent/"+Version {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if len(m.Characters) != 1 || m.Characters[0].Filename != "Kristan.d2s" || m.Characters[0].SHA256 != "aa" {
		t.Fatalf("characters not parsed: %+v", m.Characters)
	}
	if len(m.SharedStashes) != 1 || m.SharedStashes[0].Filename != "SharedStashSoftCoreV2.d2i" {
		t.Fatalf("shared stashes not parsed: %+v", m.SharedStashes)
	}
}

func TestFetchManifest503IsPullDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"pull temporarily disabled"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.FetchManifest()
	var pd *PullDisabledError
	if !errors.As(err, &pd) {
		t.Fatalf("expected PullDisabledError, got %v", err)
	}
	if pd.Message != "pull temporarily disabled" {
		t.Fatalf("PullDisabledError message = %q", pd.Message)
	}
}

func TestFetchManifestOtherErrorIsNotPullDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.FetchManifest()
	if err == nil {
		t.Fatal("expected an error on 500")
	}
	var pd *PullDisabledError
	if errors.As(err, &pd) {
		t.Fatal("500 must not be treated as pull-disabled")
	}
}

func TestDownloadUsesPathVerbatimAndReturnsSha(t *testing.T) {
	// A download_path with an already-encoded segment must be used verbatim.
	const downloadPath = "/api/v1/characters/Kri%20stan/current?v=2"
	payload := []byte("\x55\xAA\x55\xAAsynthetic-bytes")

	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.Header().Set("X-Sha256", "server-sha")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(payload)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	data, sha, err := c.Download(downloadPath)
	if err != nil {
		t.Fatalf("Download failed: %v", err)
	}
	if gotURI != downloadPath {
		t.Fatalf("server saw URI %q, want verbatim %q", gotURI, downloadPath)
	}
	if string(data) != string(payload) {
		t.Fatalf("downloaded bytes mismatch")
	}
	if sha != "server-sha" {
		t.Fatalf("X-Sha256 = %q, want server-sha", sha)
	}
}

func TestDownloadErrorStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"nope"}`)
		}))
		c := NewClient(srv.URL, "tok")
		_, _, err := c.Download("/api/v1/characters/X/current")
		srv.Close()
		if err == nil {
			t.Fatalf("expected error on HTTP %d", status)
		}
	}
}

func TestUploadSnapshotOmitsSetCurrent(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		io.WriteString(w, `{"status":"created"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.UploadSnapshot("Hero.d2s", []byte("x"), "sha", "MachineA"); err != nil {
		t.Fatalf("UploadSnapshot failed: %v", err)
	}
	if _, present := body["set_current"]; present {
		t.Fatalf("default upload must omit set_current, body = %+v", body)
	}
}

func TestUploadSnapshotBackupSetsCurrentFalse(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		io.WriteString(w, `{"status":"created"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.UploadSnapshotBackup("Hero.d2s", []byte("x"), "sha", "MachineA"); err != nil {
		t.Fatalf("UploadSnapshotBackup failed: %v", err)
	}
	v, present := body["set_current"]
	if !present {
		t.Fatalf("backup upload must include set_current, body = %+v", body)
	}
	if b, ok := v.(bool); !ok || b {
		t.Fatalf("set_current = %v, want false", v)
	}
}
