package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keyanfayaz/git-ooh/internal/service"
)

//go:embed static/*
var staticFiles embed.FS

type Info struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Token     string    `json:"token"`
	StartedAt time.Time `json:"startedAt"`
}

type Server struct {
	svc       *service.Service
	token     string
	version   string
	logger    *slog.Logger
	http      *http.Server
	mu        sync.Mutex
	terminals *terminalManager
}

func New(svc *service.Service, version string) (*Server, error) {
	token, err := service.RandomToken()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(svc.StateDir, "server.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	server := &Server{svc: svc, token: token, version: version, logger: slog.New(slog.NewJSONHandler(logFile, nil))}
	server.terminals = newTerminalManager(svc)
	return server, nil
}

func (s *Server) Run(ctx context.Context, noOpen bool, onReady func(string)) (string, error) {
	defer s.terminals.close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	info := Info{PID: os.Getpid(), Port: port, Token: s.token, StartedAt: time.Now().UTC()}
	data, _ := json.MarshalIndent(info, "", "  ")
	serverPath := filepath.Join(s.svc.StateDir, "server.json")
	if err := os.WriteFile(serverPath, data, 0o600); err != nil {
		listener.Close()
		return "", err
	}
	defer os.Remove(serverPath)
	mux := s.routes()
	s.http = &http.Server{Handler: s.securityHeaders(s.logging(mux)), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/#token=%s", port, s.token)
	if onReady != nil {
		onReady(url)
	}
	if !noOpen {
		_ = openBrowser(url)
	}
	go s.watchWorkspaces(ctx)
	go s.watchDrafts(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
	}()
	err = s.http.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return url, err
}

func ExistingURL(ctx context.Context, stateDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, "server.json"))
	if err != nil {
		return "", false
	}
	var info Info
	if json.Unmarshal(data, &info) != nil || info.Port < 1 || info.Token == "" {
		return "", false
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/health", info.Port), nil)
	req.Header.Set("X-Ooh-Token", info.Token)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d/#token=%s", info.Port, info.Token), true
}

func OpenURL(url string) error { return openBrowser(url) }

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return errors.New("automatic browser opening is supported on macOS and Linux")
	}
	return cmd.Start()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
	})
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("POST /api/tasks/draft", s.handleCreateDraftTask)
	mux.HandleFunc("POST /api/tasks/import", s.handleImportTask)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.handleRenameTask)
	mux.HandleFunc("DELETE /api/tasks/{id}/draft", s.handleDiscardDraftTask)
	mux.HandleFunc("POST /api/tasks/{id}/start", s.handleStartTask)
	mux.HandleFunc("GET /api/agents", s.handleAgents)
	mux.HandleFunc("POST /api/tasks/{id}/archive", s.handleArchiveTask)
	mux.HandleFunc("POST /api/tasks/{id}/restore", s.handleRestoreTask)
	mux.HandleFunc("POST /api/paths/{id}/fork", s.handleForkPath)
	mux.HandleFunc("POST /api/paths/{id}/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("POST /api/paths/{id}/ready", s.handleReady)
	mux.HandleFunc("POST /api/paths/{id}/restore", s.handleRestorePath)
	mux.HandleFunc("POST /api/paths/{id}/continue", s.handleContinuePath)
	mux.HandleFunc("POST /api/paths/{id}/variations", s.handleAddVariations)
	mux.HandleFunc("GET /api/paths/{id}/tree", s.handleWorkspaceTree)
	mux.HandleFunc("GET /api/paths/{id}/file", s.handleWorkspaceFile)
	mux.HandleFunc("PUT /api/paths/{id}/file", s.handleWriteWorkspaceFile)
	mux.HandleFunc("GET /api/paths/{id}/raw", s.handleWorkspaceRaw)
	mux.HandleFunc("POST /api/paths/{id}/entries", s.handleCreateWorkspaceEntry)
	mux.HandleFunc("PATCH /api/paths/{id}/entries", s.handleRenameWorkspaceEntry)
	mux.HandleFunc("DELETE /api/paths/{id}/entries", s.handleDeleteWorkspaceEntry)
	mux.HandleFunc("GET /api/paths/{id}/search", s.handleWorkspaceSearch)
	mux.HandleFunc("GET /api/paths/{id}/preview", s.handlePreviewCapability)
	mux.HandleFunc("POST /api/paths/{id}/preview", s.handleStartPreview)
	mux.HandleFunc("POST /api/paths/{id}/terminals", s.handleCreateTerminal)
	mux.HandleFunc("POST /api/runs/{id}/stop", s.handleStopRun)
	mux.HandleFunc("GET /api/runs/{id}/log", s.handleRunLog)
	mux.HandleFunc("GET /api/previews/{id}", s.handlePreview)
	mux.HandleFunc("DELETE /api/previews/{id}", s.handleStopPreview)
	mux.HandleFunc("GET /ws/terminals/{id}", s.handleTerminalSocket)
	mux.HandleFunc("POST /api/select/{id}", s.handleSelect)
	mux.HandleFunc("GET /api/checkpoints/{id}/diff", s.handleCheckpointDiff)
	mux.HandleFunc("GET /api/checkpoints/{id}/comments", s.handleComments)
	mux.HandleFunc("POST /api/checkpoints/{id}/comments", s.handleAddComment)
	mux.HandleFunc("POST /api/comments/{id}/resolve", s.handleResolveComment)
	mux.HandleFunc("POST /api/comments/{id}/reopen", s.handleReopenComment)
	mux.HandleFunc("POST /api/checkpoints/{id}/artifacts", s.handleArtifactUpload)
	mux.HandleFunc("GET /api/artifacts/{id}", s.handleArtifact)
	mux.HandleFunc("POST /api/integrations/prepare", s.handlePrepareIntegration)
	mux.HandleFunc("GET /api/integrations/{id}", s.handleCandidate)
	mux.HandleFunc("GET /api/integrations/{id}/diff", s.handleCandidateDiff)
	mux.HandleFunc("POST /api/integrations/{id}/resolve", s.handleResolveCandidate)
	mux.HandleFunc("POST /api/integrations/{id}/apply", s.handleApplyIntegration)
	mux.HandleFunc("POST /api/undo/{id}", s.handleUndo)
	mux.HandleFunc("GET /api/archives", s.handleArchives)
	mux.HandleFunc("POST /api/archives/{id}/keep", s.handleKeepArchive)
	mux.HandleFunc("POST /api/archives/cleanup", s.handleCleanup)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.Handle("/", spaHandler())
	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Header.Get("X-Ooh-Token") != s.token {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid local session token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self' ws:; frame-src http://127.0.0.1:* http://localhost:*; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
		}
	})
}

func spaHandler() http.Handler {
	root, _ := fs.Sub(staticFiles, "static")
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
		if path == "." || path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(root, path); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"repoId": s.svc.RepoID, "repoName": filepath.Base(s.svc.Repo.Root), "repoRoot": s.svc.Repo.Root, "version": s.version, "retentionDays": s.svc.Store.RetentionDays(context.Background())})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.svc.ListTasks(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	service.SortTasksByState(tasks)
	writeJSON(w, 200, tasks)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var input struct{ Title, Description, Base string }
	if !decodeJSON(w, r, &input) {
		return
	}
	detail, err := s.svc.CreateTask(r.Context(), s.svc.Repo.Root, input.Title, input.Description, input.Base)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, detail)
}

func (s *Server) handleCreateDraftTask(w http.ResponseWriter, r *http.Request) {
	detail, err := s.svc.CreateDraftTask(r.Context(), s.svc.Repo.Root)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, detail)
}

func (s *Server) handleDiscardDraftTask(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DiscardDraftTask(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"discarded": true})
}

func (s *Server) handleStartTask(w http.ResponseWriter, r *http.Request) {
	var input service.StartTaskInput
	if !decodeJSON(w, r, &input) {
		return
	}
	detail, err := s.svc.StartTask(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, detail)
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, service.AvailableAgents(r.Context()))
}

func (s *Server) handleImportTask(w http.ResponseWriter, r *http.Request) {
	var input struct{ Source, Title, Description, Base string }
	if !decodeJSON(w, r, &input) {
		return
	}
	detail, err := s.svc.ImportTask(r.Context(), s.svc.Repo.Root, input.Source, input.Title, input.Description, input.Base)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, detail)
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	detail, err := s.svc.TaskDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, detail)
}

func (s *Server) handleRenameTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	task, err := s.svc.RenameTask(r.Context(), r.PathValue("id"), input.Title)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleArchiveTask(w http.ResponseWriter, r *http.Request) {
	result, err := s.svc.ArchiveTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleRestoreTask(w http.ResponseWriter, r *http.Request) {
	result, err := s.svc.RestoreTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleForkPath(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Store.GetPath(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, err)
		return
	}
	var input struct {
		Names   []string `json:"names"`
		Summary string   `json:"summary"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := s.svc.ForkPath(r.Context(), p, input.Names, input.Summary)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, result)
}

func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	s.createCheckpoint(w, r, false)
}
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) { s.createCheckpoint(w, r, true) }
func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request, ready bool) {
	p, err := s.svc.Store.GetPath(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, err)
		return
	}
	var input service.CheckpointInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Ready = ready
	cp, err := s.svc.CreateCheckpoint(r.Context(), p, input)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, cp)
}

func (s *Server) handleRestorePath(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.RestorePath(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) handleContinuePath(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		Prompt   string `json:"prompt"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	run, err := s.svc.ContinuePath(r.Context(), r.PathValue("id"), input.Provider, input.Prompt)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleAddVariations(w http.ResponseWriter, r *http.Request) {
	var input service.AddVariationsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	detail, err := s.svc.AddVariations(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, detail)
}

func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.Store.GetAgentRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	if err := s.svc.Runs.Stop(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
}

func (s *Server) handleRunLog(w http.ResponseWriter, r *http.Request) {
	if _, err := s.svc.Store.GetAgentRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	data, next, err := s.svc.Runs.ReadLog(r.PathValue("id"), after)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(data), "next": next})
}

func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	entries, err := s.svc.ListWorkspaceTree(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	file, err := s.svc.ReadWorkspaceFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func (s *Server) handleWriteWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Revision string `json:"revision"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	file, err := s.svc.WriteWorkspaceFile(r.Context(), r.PathValue("id"), input.Path, input.Content, input.Revision)
	if errors.Is(err, service.ErrFileConflict) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, file)
}

func (s *Server) handleWorkspaceRaw(w http.ResponseWriter, r *http.Request) {
	file, info, err := s.svc.OpenWorkspaceRaw(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer file.Close()
	if mediaType := mime.TypeByExtension(filepath.Ext(info.Name())); mediaType != "" {
		w.Header().Set("Content-Type", mediaType)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *Server) handleCreateWorkspaceEntry(w http.ResponseWriter, r *http.Request) {
	var input struct{ Path, Kind string }
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.svc.CreateWorkspaceEntry(r.Context(), r.PathValue("id"), input.Path, input.Kind); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"created": true})
}

func (s *Server) handleRenameWorkspaceEntry(w http.ResponseWriter, r *http.Request) {
	var input struct{ Path, Destination string }
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.svc.RenameWorkspaceEntry(r.Context(), r.PathValue("id"), input.Path, input.Destination); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"renamed": true})
}

func (s *Server) handleDeleteWorkspaceEntry(w http.ResponseWriter, r *http.Request) {
	var input struct{ Path string }
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.svc.DeleteWorkspaceEntry(r.Context(), r.PathValue("id"), input.Path); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleWorkspaceSearch(w http.ResponseWriter, r *http.Request) {
	results, err := s.svc.SearchWorkspace(r.Context(), r.PathValue("id"), r.URL.Query().Get("q"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handlePreviewCapability(w http.ResponseWriter, r *http.Request) {
	capability, err := s.svc.DetectPreview(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capability)
}

func (s *Server) handleStartPreview(w http.ResponseWriter, r *http.Request) {
	var input service.PreviewStartInput
	if !decodeJSON(w, r, &input) {
		return
	}
	session, err := s.svc.Previews.Start(r.Context(), r.PathValue("id"), input)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, session)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	session, err := s.svc.Previews.Get(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) handleStopPreview(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Previews.Stop(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
}

func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	result, err := s.svc.SelectResult(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleCheckpointDiff(w http.ResponseWriter, r *http.Request) {
	result, err := s.svc.CheckpointDiff(r.Context(), r.PathValue("id"), r.URL.Query().Get("baseline"), r.URL.Query().Get("file"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleComments(w http.ResponseWriter, r *http.Request) {
	comments, err := s.svc.Store.ListComments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, comments)
}

func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
	var input service.CommentInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.CheckpointID = r.PathValue("id")
	c, err := s.svc.AddComment(r.Context(), input)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) handleResolveComment(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResolveComment(r.Context(), r.PathValue("id"), true); err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"resolved": true})
}

func (s *Server) handleReopenComment(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResolveComment(r.Context(), r.PathValue("id"), false); err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"resolved": false})
}

func (s *Server) handleArtifactUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 26<<20)
	if err := r.ParseMultipartForm(26 << 20); err != nil {
		writeError(w, 422, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 422, err)
		return
	}
	defer file.Close()
	a, err := s.svc.AddArtifact(r.Context(), r.PathValue("id"), header.Filename, file, header.Size)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	a, err := s.svc.Store.GetArtifact(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", a.MediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", a.Name))
	http.ServeFile(w, r, a.StoragePath)
}

func (s *Server) handlePrepareIntegration(w http.ResponseWriter, r *http.Request) {
	var input struct{ TaskID, CheckpointID, CommitMessage string }
	if !decodeJSON(w, r, &input) {
		return
	}
	c, err := s.svc.PrepareIntegration(r.Context(), input.TaskID, input.CheckpointID, input.CommitMessage)
	if err != nil && c.ID == "" {
		writeError(w, 422, err)
		return
	}
	status := http.StatusCreated
	if err != nil {
		status = http.StatusConflict
	}
	writeJSON(w, status, c)
}

func (s *Server) handleCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.Store.GetCandidate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleCandidateDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := s.svc.CandidateDiff(r.Context(), r.PathValue("id"), r.URL.Query().Get("file"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, diff)
}

func (s *Server) handleResolveCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.ResolveCandidate(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) handleApplyIntegration(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ConfirmOID string `json:"confirmOid"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	i, err := s.svc.ApplyIntegration(r.Context(), r.PathValue("id"), input.ConfirmOID)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, i)
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	i, err := s.svc.UndoIntegration(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, i)
}

func (s *Server) handleArchives(w http.ResponseWriter, r *http.Request) {
	paths, err := s.svc.ArchivedPaths(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, paths)
}

func (s *Server) handleKeepArchive(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Keep bool `json:"keep"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	p, err := s.svc.SetPathKeep(r.Context(), r.PathValue("id"), input.Keep)
	if err != nil {
		writeError(w, 422, err)
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	ids, err := s.svc.CleanupExpired(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deletedPathIds": ids})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, errors.New("streaming unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, unsubscribe := s.svc.Bus.Subscribe()
	defer unsubscribe()
	fmt.Fprint(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: state\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) watchWorkspaces(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tasks, err := s.svc.ListTasks(ctx)
			if err != nil {
				continue
			}
			var fingerprint strings.Builder
			for _, task := range tasks {
				detail, err := s.svc.TaskDetail(ctx, task.ID)
				if err != nil {
					continue
				}
				for pathID, status := range detail.Statuses {
					fingerprint.WriteString(pathID + status.HeadOID + strconv.FormatBool(status.Dirty) + strconv.Itoa(status.Changed))
				}
			}
			current := fingerprint.String()
			if last != "" && current != last {
				s.svc.Bus.Publish("workspace.changed", "")
			}
			last = current
		}
	}
}

func (s *Server) watchDrafts(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.svc.CleanupDrafts(ctx, time.Now().UTC().Add(-24*time.Hour))
		}
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeError(w, 400, fmt.Errorf("invalid JSON request: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": "1", "data": data})
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": "1", "error": map[string]any{"code": errorCode(status), "message": err.Error()}})
}

func writeServiceError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	if service.IsStoreNotFound(err) || errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	} else if errors.Is(err, service.ErrFileConflict) || strings.Contains(strings.ToLower(err.Error()), "already") || strings.Contains(strings.ToLower(err.Error()), "active") {
		status = http.StatusConflict
	}
	writeError(w, status, err)
}

func errorCode(status int) string {
	switch status {
	case 400:
		return "usage_error"
	case 401:
		return "unauthorized"
	case 404:
		return "not_found"
	case 409:
		return "state_conflict"
	case 422:
		return "precondition_failed"
	default:
		return "internal_error"
	}
}
