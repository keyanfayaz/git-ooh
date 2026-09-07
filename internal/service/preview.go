package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keyanfayaz/git-ooh/internal/id"
	"github.com/keyanfayaz/git-ooh/internal/model"
	"github.com/keyanfayaz/git-ooh/internal/store"
)

type PreviewCapability struct {
	Available        bool   `json:"available"`
	Kind             string `json:"kind,omitempty"`
	InstallCommand   string `json:"installCommand,omitempty"`
	RunCommand       string `json:"runCommand,omitempty"`
	ExpectedURL      string `json:"expectedUrl,omitempty"`
	RequiresApproval bool   `json:"requiresApproval"`
	Reason           string `json:"reason,omitempty"`
}

type PreviewSession struct {
	ID        string    `json:"id"`
	PathID    string    `json:"pathId"`
	State     string    `json:"state"`
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	Log       string    `json:"log,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

type PreviewStartInput struct {
	InstallCommand string `json:"installCommand"`
	RunCommand     string `json:"runCommand"`
	ExpectedURL    string `json:"expectedUrl"`
	Approved       bool   `json:"approved"`
}

type PreviewManager struct {
	svc      *Service
	mu       sync.Mutex
	sessions map[string]*PreviewSession
	commands map[string]*exec.Cmd
	wg       sync.WaitGroup
}

func NewPreviewManager(svc *Service) *PreviewManager {
	return &PreviewManager{svc: svc, sessions: map[string]*PreviewSession{}, commands: map[string]*exec.Cmd{}}
}

func (m *PreviewManager) Close() {
	m.mu.Lock()
	commands := make([]*exec.Cmd, 0, len(m.commands))
	for _, command := range m.commands {
		commands = append(commands, command)
	}
	m.mu.Unlock()
	for _, command := range commands {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	m.wg.Wait()
}

func (s *Service) DetectPreview(ctx context.Context, pathID string) (PreviewCapability, error) {
	p, err := s.Store.GetPath(ctx, pathID)
	if err != nil {
		return PreviewCapability{}, err
	}
	root := p.WorktreePath
	capability := detectRunnable(root)
	if !capability.Available {
		return capability, nil
	}
	if saved, savedErr := s.Store.Setting(ctx, "preview_approval"); savedErr == nil {
		var approved PreviewStartInput
		if json.Unmarshal([]byte(saved), &approved) == nil && approved.RunCommand == capability.RunCommand && approved.InstallCommand == capability.InstallCommand {
			capability.RequiresApproval = false
		}
	}
	return capability, nil
}

func detectRunnable(root string) PreviewCapability {
	if data, err := os.ReadFile(filepath.Join(root, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil {
			manager, install := "npm", "npm install"
			if fileExists(filepath.Join(root, "pnpm-lock.yaml")) {
				manager, install = "pnpm", "pnpm install"
			} else if fileExists(filepath.Join(root, "yarn.lock")) {
				manager, install = "yarn", "yarn install"
			} else if fileExists(filepath.Join(root, "bun.lockb")) || fileExists(filepath.Join(root, "bun.lock")) {
				manager, install = "bun", "bun install"
			}
			for _, script := range []string{"dev", "start", "serve", "preview"} {
				if pkg.Scripts[script] != "" {
					if fileExists(filepath.Join(root, "node_modules")) {
						install = ""
					}
					return PreviewCapability{Available: true, Kind: "JavaScript application", InstallCommand: install, RunCommand: manager + " run " + script, ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
				}
			}
		}
	}
	if fileExists(filepath.Join(root, "index.html")) {
		return PreviewCapability{Available: true, Kind: "Static website", RunCommand: "python3 -m http.server ${PORT} --bind 127.0.0.1", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "manage.py")) {
		return PreviewCapability{Available: true, Kind: "Django application", InstallCommand: pythonInstall(root), RunCommand: "python3 manage.py runserver 127.0.0.1:${PORT}", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "app.py")) {
		return PreviewCapability{Available: true, Kind: "Python application", InstallCommand: pythonInstall(root), RunCommand: "python3 app.py", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "main.py")) {
		return PreviewCapability{Available: true, Kind: "Python application", InstallCommand: pythonInstall(root), RunCommand: "python3 main.py", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "Gemfile")) {
		command := "bundle exec ruby app.rb"
		kind := "Ruby application"
		if fileExists(filepath.Join(root, "bin", "rails")) {
			command, kind = "bin/rails server -b 127.0.0.1 -p ${PORT}", "Rails application"
		}
		return PreviewCapability{Available: true, Kind: kind, InstallCommand: "bundle install", RunCommand: command, ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "go.mod")) {
		return PreviewCapability{Available: true, Kind: "Go application", RunCommand: "go run .", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "Cargo.toml")) {
		return PreviewCapability{Available: true, Kind: "Rust application", RunCommand: "cargo run", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "compose.yaml")) || fileExists(filepath.Join(root, "compose.yml")) || fileExists(filepath.Join(root, "docker-compose.yml")) {
		return PreviewCapability{Available: true, Kind: "Docker Compose application", RunCommand: "docker compose up", ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
	}
	if fileExists(filepath.Join(root, "Makefile")) {
		data, _ := os.ReadFile(filepath.Join(root, "Makefile"))
		for _, target := range []string{"dev", "serve", "run", "start"} {
			if regexp.MustCompile(`(?m)^` + target + `\s*:`).Match(data) {
				return PreviewCapability{Available: true, Kind: "Runnable project", RunCommand: "make " + target, ExpectedURL: "http://127.0.0.1:${PORT}", RequiresApproval: true}
			}
		}
	}
	return PreviewCapability{Available: false, Reason: "Ooh could not find a familiar run command. The files and terminal are still available."}
}

func pythonInstall(root string) string {
	if fileExists(filepath.Join(root, "uv.lock")) {
		return "uv sync"
	}
	if fileExists(filepath.Join(root, "requirements.txt")) {
		return "python3 -m pip install -r requirements.txt"
	}
	if fileExists(filepath.Join(root, "pyproject.toml")) {
		return "python3 -m pip install -e ."
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (m *PreviewManager) Start(ctx context.Context, pathID string, input PreviewStartInput) (PreviewSession, error) {
	p, err := m.svc.Store.GetPath(ctx, pathID)
	if err != nil {
		return PreviewSession{}, err
	}
	input.InstallCommand = strings.TrimSpace(input.InstallCommand)
	input.RunCommand = strings.TrimSpace(input.RunCommand)
	if input.RunCommand == "" {
		return PreviewSession{}, errors.New("a preview command is required")
	}
	if !input.Approved {
		return PreviewSession{}, errors.New("confirm the local commands before running Preview")
	}
	data, _ := json.Marshal(input)
	_ = m.svc.Store.SetSetting(ctx, "preview_approval", string(data))
	port, err := freeLoopbackPort()
	if err != nil {
		return PreviewSession{}, err
	}
	replacer := strings.NewReplacer("${PORT}", strconv.Itoa(port), "$PORT", strconv.Itoa(port))
	input.InstallCommand = replacer.Replace(input.InstallCommand)
	input.RunCommand = replacer.Replace(input.RunCommand)
	input.ExpectedURL = replacer.Replace(input.ExpectedURL)
	if input.ExpectedURL == "" {
		input.ExpectedURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	if !loopbackURL(input.ExpectedURL) {
		return PreviewSession{}, errors.New("Preview URLs must use localhost or 127.0.0.1")
	}
	session := &PreviewSession{ID: id.New("preview"), PathID: pathID, State: "starting", URL: input.ExpectedURL, StartedAt: time.Now().UTC()}
	m.mu.Lock()
	for existingID, existing := range m.sessions {
		if existing.PathID == pathID && (existing.State == "starting" || existing.State == "running") {
			if command := m.commands[existingID]; command != nil && command.Process != nil {
				_ = command.Process.Kill()
			}
			existing.State = "stopped"
		}
	}
	m.sessions[session.ID] = session
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(*session, p, input, port)
	}()
	return *session, nil
}

func (m *PreviewManager) run(session PreviewSession, path model.Path, input PreviewStartInput, port int) {
	appendLog := func(text string) {
		m.mu.Lock()
		if current := m.sessions[session.ID]; current != nil {
			current.Log += text
			if len(current.Log) > 256<<10 {
				current.Log = current.Log[len(current.Log)-(256<<10):]
			}
		}
		m.mu.Unlock()
		m.svc.Bus.Publish("preview.output", session.ID)
	}
	if input.InstallCommand != "" {
		appendLog("Preparing dependencies…\n")
		command := shellCommand(input.InstallCommand, path.WorktreePath)
		command.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "HOST=127.0.0.1")
		m.mu.Lock()
		m.commands[session.ID] = command
		m.mu.Unlock()
		output, err := command.CombinedOutput()
		m.mu.Lock()
		delete(m.commands, session.ID)
		m.mu.Unlock()
		appendLog(string(output))
		if err != nil {
			m.setPreviewError(session.ID, "Dependency setup failed: "+err.Error())
			return
		}
	}
	command := shellCommand(input.RunCommand, path.WorktreePath)
	command.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "HOST=127.0.0.1", "BROWSER=none")
	stdout, err := command.StdoutPipe()
	if err != nil {
		m.setPreviewError(session.ID, err.Error())
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		m.setPreviewError(session.ID, err.Error())
		return
	}
	m.mu.Lock()
	m.commands[session.ID] = command
	m.mu.Unlock()
	if err := command.Start(); err != nil {
		m.setPreviewError(session.ID, err.Error())
		return
	}
	m.mu.Lock()
	if current := m.sessions[session.ID]; current != nil {
		current.State = "running"
	}
	m.mu.Unlock()
	m.svc.Bus.Publish("preview.running", session.ID)
	consume := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			appendLog(line)
			if found := firstLoopbackURL(line); found != "" {
				m.mu.Lock()
				if current := m.sessions[session.ID]; current != nil {
					current.URL = found
				}
				m.mu.Unlock()
			}
		}
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); consume(stdout) }()
	go func() { defer readers.Done(); consume(stderr) }()
	err = command.Wait()
	readers.Wait()
	m.mu.Lock()
	delete(m.commands, session.ID)
	current := m.sessions[session.ID]
	if current != nil && current.State != "stopped" {
		if err != nil {
			current.State, current.Error = "failed", err.Error()
		} else {
			current.State = "stopped"
		}
	}
	m.mu.Unlock()
	m.svc.Bus.Publish("preview.stopped", session.ID)
}

func (m *PreviewManager) Get(sessionID string) (PreviewSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil {
		return PreviewSession{}, store.ErrNotFound
	}
	return *session, nil
}

func (m *PreviewManager) Stop(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil {
		return store.ErrNotFound
	}
	if command := m.commands[sessionID]; command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	session.State = "stopped"
	return nil
}

func (m *PreviewManager) setPreviewError(sessionID, message string) {
	m.mu.Lock()
	if session := m.sessions[sessionID]; session != nil {
		session.State, session.Error = "failed", message
	}
	m.mu.Unlock()
	m.svc.Bus.Publish("preview.failed", sessionID)
}

func shellCommand(command, dir string) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		if runtime.GOOS == "windows" {
			return exec.Command("cmd", "/C", command)
		}
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-lc", command)
	cmd.Dir = dir
	return cmd
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

var urlPattern = regexp.MustCompile(`https?://(?:127\.0\.0\.1|localhost):[0-9]+(?:/[^\s]*)?`)

func firstLoopbackURL(text string) string {
	found := urlPattern.FindString(text)
	if loopbackURL(found) {
		return found
	}
	return ""
}

func loopbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
