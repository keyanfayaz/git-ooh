package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/keyanfayaz/git-ooh/internal/id"
	"github.com/keyanfayaz/git-ooh/internal/service"
)

type terminalTicket struct {
	ID        string
	PathID    string
	Workspace string
	Ticket    string
	ExpiresAt time.Time
}

type terminalSession struct {
	command *exec.Cmd
	pty     *os.File
}

type terminalManager struct {
	svc      *service.Service
	mu       sync.Mutex
	tickets  map[string]terminalTicket
	sessions map[string]*terminalSession
}

func newTerminalManager(svc *service.Service) *terminalManager {
	return &terminalManager{svc: svc, tickets: map[string]terminalTicket{}, sessions: map[string]*terminalSession{}}
}

func (m *terminalManager) close() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.pty.Close()
		if session.command.Process != nil {
			_ = session.command.Process.Kill()
		}
	}
}

func (s *Server) handleCreateTerminal(w http.ResponseWriter, r *http.Request) {
	path, err := s.svc.Store.GetPath(r.Context(), r.PathValue("id"))
	if err != nil || path.WorktreePath == "" {
		writeServiceError(w, err)
		return
	}
	ticket, err := service.RandomToken()
	if err != nil {
		writeError(w, 500, err)
		return
	}
	idValue := id.New("terminal")
	s.terminals.mu.Lock()
	s.terminals.tickets[idValue] = terminalTicket{ID: idValue, PathID: path.ID, Workspace: path.WorktreePath, Ticket: ticket, ExpiresAt: time.Now().Add(30 * time.Second)}
	s.terminals.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"id": idValue, "ticket": ticket})
}

func (s *Server) handleTerminalSocket(w http.ResponseWriter, r *http.Request) {
	idValue := r.PathValue("id")
	ticketValue := r.URL.Query().Get("ticket")
	s.terminals.mu.Lock()
	ticket, ok := s.terminals.tickets[idValue]
	if ok {
		delete(s.terminals.tickets, idValue)
	}
	s.terminals.mu.Unlock()
	if !ok || ticket.Ticket != ticketValue || time.Now().After(ticket.ExpiresAt) {
		http.Error(w, "invalid or expired terminal ticket", http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(request *http.Request) bool { return request.Host == r.Host }}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()

	command := terminalCommand(ticket.Workspace)
	command.Env = append(os.Environ(), "TERM=xterm-256color", "OOH_REPO_ROOT="+s.svc.Repo.Root, "OOH_PATH_ID="+ticket.PathID)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 100})
	if err != nil {
		_ = connection.WriteMessage(websocket.TextMessage, []byte("Unable to start terminal: "+err.Error()))
		return
	}
	session := &terminalSession{command: command, pty: terminal}
	s.terminals.mu.Lock()
	s.terminals.sessions[idValue] = session
	s.terminals.mu.Unlock()
	defer func() {
		_ = terminal.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		s.terminals.mu.Lock()
		delete(s.terminals.sessions, idValue)
		s.terminals.mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := terminal.Read(buffer)
			if n > 0 {
				if connection.WriteMessage(websocket.BinaryMessage, buffer[:n]) != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	for {
		_, data, readErr := connection.ReadMessage()
		if readErr != nil {
			break
		}
		var message struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		switch message.Type {
		case "input":
			_, _ = io.WriteString(terminal, message.Data)
		case "resize":
			if message.Cols > 0 && message.Rows > 0 && message.Cols < 1000 && message.Rows < 1000 {
				_ = pty.Setsize(terminal, &pty.Winsize{Cols: uint16(message.Cols), Rows: uint16(message.Rows)})
			}
		}
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
}

func terminalCommand(dir string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		command := exec.Command("cmd")
		command.Dir = dir
		return command
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	command := exec.Command(shell, "-l")
	command.Dir = dir
	return command
}
