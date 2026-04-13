package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaybeUploadAuthFileToCPASkipsWhenURLMissing(t *testing.T) {
	t.Parallel()

	attempted, err := maybeUploadAuthFileToCPA(context.Background(), config{}, "demo@example.com", "auth/demo.json")
	if err != nil {
		t.Fatalf("expected skip without error, got %v", err)
	}
	if attempted {
		t.Fatal("expected upload to be skipped when cpa-url is empty")
	}
}

func TestMaybeUploadAuthFileToCPASkipsWhenKeyMissing(t *testing.T) {
	t.Parallel()

	attempted, err := maybeUploadAuthFileToCPA(context.Background(), config{cpaURL: "http://127.0.0.1:8317"}, "demo@example.com", "auth/demo.json")
	if err != nil {
		t.Fatalf("expected skip without error, got %v", err)
	}
	if attempted {
		t.Fatal("expected upload to be skipped when cpa-key is empty")
	}
}

func TestMaybeUploadAuthFileToCPAUploadsAuthJSON(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	authFilePath := filepath.Join(tempDir, "codex-demo_example_com.json")
	authBody := []byte("{\"type\":\"codex\",\"email\":\"demo@example.com\"}")
	if err := os.WriteFile(authFilePath, authBody, 0o644); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotAuth   string
		gotType   string
		gotBody   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		gotPath = request.URL.Path
		gotQuery = request.URL.RawQuery
		gotAuth = request.Header.Get("Authorization")
		gotType = request.Header.Get("Content-Type")
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotBody = string(body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"data":{}}`))
	}))
	defer server.Close()

	attempted, err := maybeUploadAuthFileToCPA(context.Background(), config{
		cpaURL:         server.URL,
		cpaKey:         "secret-key",
		requestTimeout: 2 * time.Second,
	}, "demo@example.com", authFilePath)
	if err != nil {
		t.Fatalf("expected upload success, got %v", err)
	}
	if !attempted {
		t.Fatal("expected upload attempt to happen")
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v0/management/auth-files" {
		t.Fatalf("path = %s, want /v0/management/auth-files", gotPath)
	}
	if gotQuery != "name=codex-demo_example_com.json" {
		t.Fatalf("query = %s, want name=codex-demo_example_com.json", gotQuery)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("authorization = %s, want Bearer secret-key", gotAuth)
	}
	if gotType != "application/json" {
		t.Fatalf("content-type = %s, want application/json", gotType)
	}
	if gotBody != string(authBody) {
		t.Fatalf("body = %s, want %s", gotBody, string(authBody))
	}
}
