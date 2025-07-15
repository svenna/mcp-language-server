package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/isaacphi/mcp-language-server/internal/logging"
	"github.com/isaacphi/mcp-language-server/internal/lsp"
	"github.com/isaacphi/mcp-language-server/internal/watcher"
	"github.com/mark3labs/mcp-go/server"
)

// Create a logger for the core component
var coreLogger = logging.NewLogger(logging.Core)

type config struct {
	workspaceDir string
	lspCommand   string
	openGlobs    StringArrayFlag
	lspArgs      []string
	// Transport configuration
	transport string // "stdio", "sse", "http"
	host      string // network interface for network transports
	port      int    // network port for network transports
	endpoint  string // HTTP endpoint path for http transport
}

type mcpServer struct {
	config           config
	lspClient        *lsp.Client
	mcpServer        *server.MCPServer
	ctx              context.Context
	cancelFunc       context.CancelFunc
	workspaceWatcher *watcher.WorkspaceWatcher
}

// StringArrayFlag is a custom flag type to handle an array of strings
type StringArrayFlag []string

// Set appends a new value to the custom flag value
func (s *StringArrayFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// String returns the string representation of the custom flag value
func (s *StringArrayFlag) String() string {
	return strings.Join(*s, ",")
}

func parseConfig() (*config, error) {
	cfg := &config{}
	flag.StringVar(&cfg.workspaceDir, "workspace", "", "Path to workspace directory")
	flag.StringVar(&cfg.lspCommand, "lsp", "", "LSP command to run (args should be passed after --)")
	flag.Var(&cfg.openGlobs, "open", "Glob of files to open by default (can specify more than once)")
	// Transport configuration flags
	flag.StringVar(&cfg.transport, "transport", "stdio", "Transport method: stdio, sse, or http")
	flag.StringVar(&cfg.host, "host", "localhost", "Host address for network transports")
	flag.IntVar(&cfg.port, "port", 8080, "Port for network transports")
	flag.StringVar(&cfg.endpoint, "endpoint", "/mcp", "HTTP endpoint path for http transport")
	flag.Parse()

	// Get remaining args after -- as LSP arguments
	cfg.lspArgs = flag.Args()

	// Validate workspace directory
	if cfg.workspaceDir == "" {
		return nil, fmt.Errorf("workspace directory is required")
	}

	workspaceDir, err := filepath.Abs(cfg.workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for workspace: %v", err)
	}
	cfg.workspaceDir = workspaceDir

	if _, err := os.Stat(cfg.workspaceDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace directory does not exist: %s", cfg.workspaceDir)
	}

	// Validate LSP command
	if cfg.lspCommand == "" {
		return nil, fmt.Errorf("LSP command is required")
	}

	if _, err := exec.LookPath(cfg.lspCommand); err != nil {
		return nil, fmt.Errorf("LSP command not found: %s", cfg.lspCommand)
	}

	// Validate transport configuration
	switch cfg.transport {
	case "stdio", "sse", "http":
		// Valid transport types
	default:
		return nil, fmt.Errorf("invalid transport type: %s (must be stdio, sse, or http)", cfg.transport)
	}

	// Validate network configuration for network transports
	if cfg.transport == "sse" || cfg.transport == "http" {
		if cfg.port <= 0 || cfg.port > 65535 {
			return nil, fmt.Errorf("invalid port: %d (must be 1-65535)", cfg.port)
		}
		if cfg.host == "" {
			return nil, fmt.Errorf("host is required for network transports")
		}
	}

	return cfg, nil
}

func newServer(config *config) (*mcpServer, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &mcpServer{
		config:     *config,
		ctx:        ctx,
		cancelFunc: cancel,
	}, nil
}

func (s *mcpServer) initializeLSP() error {
	if err := os.Chdir(s.config.workspaceDir); err != nil {
		return fmt.Errorf("failed to change to workspace directory: %v", err)
	}

	client, err := lsp.NewClient(s.config.lspCommand, s.config.lspArgs...)
	if err != nil {
		return fmt.Errorf("failed to create LSP client: %v", err)
	}
	s.lspClient = client
	s.workspaceWatcher = watcher.NewWorkspaceWatcher(client)

	initResult, err := client.InitializeLSPClient(s.ctx, s.config.workspaceDir)
	if err != nil {
		return fmt.Errorf("initialize failed: %v", err)
	}

	coreLogger.Debug("Server capabilities: %+v", initResult.Capabilities)

	if len(s.config.openGlobs) > 0 {
		s.openInitialFiles()
	}

	go s.workspaceWatcher.WatchWorkspace(s.ctx, s.config.workspaceDir)
	return client.WaitForServerReady(s.ctx)
}

func (s *mcpServer) openInitialFiles() {

	err := filepath.WalkDir(s.config.workspaceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			for _, pattern := range s.config.openGlobs {
				match, err := doublestar.PathMatch(pattern, path)
				if err != nil {
					return err
				}

				if match {
					if err := s.lspClient.OpenFile(s.ctx, path); err != nil {
						coreLogger.Error("Failed to open file %s: %v", path, err)
					}
					break
				}
			}
		}

		return nil
	})
	if err != nil {
		coreLogger.Error("openInitialFiles failed: %v", err)
	}
}

func (s *mcpServer) start() error {
	if err := s.initializeLSP(); err != nil {
		return err
	}

	s.mcpServer = server.NewMCPServer(
		"MCP Language Server",
		"v0.0.2",
		server.WithLogging(),
		server.WithRecovery(),
	)

	err := s.registerTools()
	if err != nil {
		return fmt.Errorf("tool registration failed: %v", err)
	}

	// Start the appropriate transport
	switch s.config.transport {
	case "stdio", "":
		coreLogger.Info("Starting MCP server with stdio transport")
		return server.ServeStdio(s.mcpServer)
	case "sse":
		addr := fmt.Sprintf("%s:%d", s.config.host, s.config.port)
		coreLogger.Info("Starting MCP server with SSE transport on %s", addr)
		sseServer := server.NewSSEServer(s.mcpServer)
		return sseServer.Start(addr)
	case "http":
		addr := fmt.Sprintf("%s:%d", s.config.host, s.config.port)
		coreLogger.Info("Starting MCP server with StreamableHTTP transport on %s%s", addr, s.config.endpoint)
		httpServer := server.NewStreamableHTTPServer(s.mcpServer,
			server.WithEndpointPath(s.config.endpoint),
		)
		return httpServer.Start(addr)
	default:
		return fmt.Errorf("unsupported transport type: %s", s.config.transport)
	}
}

func main() {
	coreLogger.Info("MCP Language Server starting")

	done := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	config, err := parseConfig()
	if err != nil {
		coreLogger.Fatal("%v", err)
	}

	srv, err := newServer(config)
	if err != nil {
		coreLogger.Fatal("%v", err)
	}

	// Parent process monitoring channel
	parentDeath := make(chan struct{})

	// Monitor parent process termination
	// Claude desktop does not properly kill child processes for MCP servers
	go func() {
		ppid := os.Getppid()
		coreLogger.Debug("Monitoring parent process: %d", ppid)

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				currentPpid := os.Getppid()
				if currentPpid != ppid && (currentPpid == 1 || ppid == 1) {
					coreLogger.Info("Parent process %d terminated (current ppid: %d), initiating shutdown", ppid, currentPpid)
					close(parentDeath)
					return
				}
			case <-done:
				return
			}
		}
	}()

	// Handle shutdown triggers
	go func() {
		select {
		case sig := <-sigChan:
			coreLogger.Info("Received signal %v in PID: %d", sig, os.Getpid())
			cleanup(srv, done)
		case <-parentDeath:
			coreLogger.Info("Parent death detected, initiating shutdown")
			cleanup(srv, done)
		}
	}()

	if err := srv.start(); err != nil {
		coreLogger.Error("Server error: %v", err)
		cleanup(srv, done)
		os.Exit(1)
	}

	<-done
	coreLogger.Info("Server shutdown complete for PID: %d", os.Getpid())
	os.Exit(0)
}

func cleanup(s *mcpServer, done chan struct{}) {
	coreLogger.Info("Cleanup initiated for PID: %d", os.Getpid())

	// Create a context with timeout for shutdown operations
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if s.lspClient != nil {
		coreLogger.Info("Closing open files")
		s.lspClient.CloseAllFiles(ctx)

		// Create a shorter timeout context for the shutdown request
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer shutdownCancel()

		// Run shutdown in a goroutine with timeout to avoid blocking if LSP doesn't respond
		shutdownDone := make(chan struct{})
		go func() {
			coreLogger.Info("Sending shutdown request")
			if err := s.lspClient.Shutdown(shutdownCtx); err != nil {
				coreLogger.Error("Shutdown request failed: %v", err)
			}
			close(shutdownDone)
		}()

		// Wait for shutdown with timeout
		select {
		case <-shutdownDone:
			coreLogger.Info("Shutdown request completed")
		case <-time.After(1 * time.Second):
			coreLogger.Warn("Shutdown request timed out, proceeding with exit")
		}

		coreLogger.Info("Sending exit notification")
		if err := s.lspClient.Exit(ctx); err != nil {
			coreLogger.Error("Exit notification failed: %v", err)
		}

		coreLogger.Info("Closing LSP client")
		if err := s.lspClient.Close(); err != nil {
			coreLogger.Error("Failed to close LSP client: %v", err)
		}
	}

	// Send signal to the done channel
	select {
	case <-done: // Channel already closed
	default:
		close(done)
	}

	coreLogger.Info("Cleanup completed for PID: %d", os.Getpid())
}
