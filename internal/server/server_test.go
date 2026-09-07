package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/service"
)

func TestLocalAPIRequiresSessionToken(t *testing.T) {
	repo := testRepository(t)
	t.Setenv("OOH_DATA_HOME", t.TempDir())
	svc, err := service.Open(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server, err := New(svc, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.securityHeaders(server.routes())

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	authorizedRequest.Header.Set("X-Ooh-Token", server.token)
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"repoName"`) {
		t.Fatalf("authorized response = %d %s", authorized.Code, authorized.Body.String())
	}
	if authorized.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("security headers were not applied")
	}

	emptyTasksRequest := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	emptyTasksRequest.Header.Set("X-Ooh-Token", server.token)
	emptyTasks := httptest.NewRecorder()
	handler.ServeHTTP(emptyTasks, emptyTasksRequest)
	if emptyTasks.Code != http.StatusOK || !strings.Contains(emptyTasks.Body.String(), `"data":[]`) {
		t.Fatalf("empty task response should use an array: %d %s", emptyTasks.Code, emptyTasks.Body.String())
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "<title>Ooh</title>") {
		t.Fatalf("embedded dashboard response = %d", page.Code)
	}
}

func TestWorkspaceAPIFlow(t *testing.T) {
	repo := testRepository(t)
	t.Setenv("OOH_DATA_HOME", t.TempDir())
	svc, err := service.Open(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server, err := New(svc, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.securityHeaders(server.routes())
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/draft", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("X-Ooh-Token", server.token)
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create draft = %d %s", created.Code, created.Body.String())
	}
	var envelope struct {
		Data model.TaskDetail `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil || len(envelope.Data.Paths) != 1 {
		t.Fatalf("decode draft: %v %s", err, created.Body.String())
	}
	pathID := envelope.Data.Paths[0].ID
	treeRequest := httptest.NewRequest(http.MethodGet, "/api/paths/"+pathID+"/tree", nil)
	treeRequest.Header.Set("X-Ooh-Token", server.token)
	tree := httptest.NewRecorder()
	handler.ServeHTTP(tree, treeRequest)
	if tree.Code != http.StatusOK || !strings.Contains(tree.Body.String(), `"README.md"`) {
		t.Fatalf("workspace tree = %d %s", tree.Code, tree.Body.String())
	}
	fileRequest := httptest.NewRequest(http.MethodGet, "/api/paths/"+pathID+"/file?path=README.md", nil)
	fileRequest.Header.Set("X-Ooh-Token", server.token)
	file := httptest.NewRecorder()
	handler.ServeHTTP(file, fileRequest)
	if file.Code != http.StatusOK || !strings.Contains(file.Body.String(), `"revision"`) {
		t.Fatalf("workspace file = %d %s", file.Code, file.Body.String())
	}
}

func testRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.name", "Ooh Test")
	runTestGit(t, dir, "config", "user.email", "ooh@example.test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "README.md")
	runTestGit(t, dir, "commit", "-m", "initial")
	return dir
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
