package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

const version = "1.2.0"

// Container and network names
const (
	proxyContainerName = "tcv-egress"
	proxyImageName     = "tcv-egress:latest"
	networkName        = "agent-net"
	policyVolumeName   = "tcv-policies"
	defaultProxyPort   = "8080"
)

// defaultBatchPrompt is used when --batch is specified without --prompt.
// It provides a sensible default for unattended agent runs.
const defaultBatchPrompt = `You are running in batch mode.

1. Read the task description or project README to understand what needs to be done.
2. Implement the changes, running tests as you go.
3. Ensure all work is committed before exiting.
4. Do not ask for human input. If you encounter an issue you cannot resolve
   after 3 attempts, document the blocker and exit.`

// hookConfig defines a lifecycle hook — a shell command run at a specific event.
// Hooks receive session metadata as JSON on stdin and as environment variables.
type hookConfig struct {
	Command string `json:"command"`           // Shell command to execute
	Timeout int    `json:"timeout,omitempty"`  // Timeout in seconds (default: 10)
}

// hooksConfig defines all available lifecycle hooks.
// Loaded from ~/.config/tcv/hooks.json (global) or .tcv.json "hooks" (per-project).
type hooksConfig struct {
	SessionStarted *hookConfig `json:"session.started,omitempty"` // After session container starts
	SessionStopped *hookConfig `json:"session.stopped,omitempty"` // After session container exits
}

// sessionEvent is the JSON payload passed to hooks via stdin.
type sessionEvent struct {
	Event         string `json:"event"`
	SessionID     string `json:"sessionId"`
	Project       string `json:"project"`
	ProjectPath   string `json:"projectPath"`
	Name          string `json:"name"`
	AgentType     string `json:"agentType"`
	Cmd           string `json:"cmd"`
	Status        string `json:"status"`
	ContainerName string `json:"containerName"`
	GitBranch     string `json:"gitBranch,omitempty"`
	GitCommit     string `json:"gitCommit,omitempty"`
	GitDirty      bool   `json:"gitDirty,omitempty"`
	Timestamp     string `json:"timestamp"`
}

// loadHooksConfig loads hook configurations.
// Priority: project .tcv.json "hooks" > user ~/.config/tcv/hooks.json
func loadHooksConfig(projectPolicy *policyConfig) *hooksConfig {
	hooks := &hooksConfig{}

	// Load user-level hooks
	userConfigDir := os.Getenv("XDG_CONFIG_HOME")
	if userConfigDir == "" {
		home, _ := os.UserHomeDir()
		userConfigDir = filepath.Join(home, ".config")
	}
	hooksFile := filepath.Join(userConfigDir, "tcv", "hooks.json")
	if data, err := os.ReadFile(hooksFile); err == nil {
		_ = json.Unmarshal(data, hooks)
	}

	// Project-level hooks override user-level
	if projectPolicy != nil && projectPolicy.Hooks != nil {
		if projectPolicy.Hooks.SessionStarted != nil {
			hooks.SessionStarted = projectPolicy.Hooks.SessionStarted
		}
		if projectPolicy.Hooks.SessionStopped != nil {
			hooks.SessionStopped = projectPolicy.Hooks.SessionStopped
		}
	}

	return hooks
}

// runHook executes a lifecycle hook if configured. The hook command receives
// session metadata as JSON on stdin and key fields as environment variables.
// Hooks run asynchronously — failures are logged but never block the CLI.
func runHook(hook *hookConfig, event sessionEvent) {
	if hook == nil || hook.Command == "" {
		return
	}

	payload, err := json.Marshal(event)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: hook marshal failed: %v\n", err)
		return
	}

	timeout := 10 * time.Second
	if hook.Timeout > 0 {
		timeout = time.Duration(hook.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	go func() {
		cmd := exec.CommandContext(ctx, "sh", "-c", hook.Command)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Env = append(os.Environ(),
			"TCV_EVENT="+event.Event,
			"TCV_SESSION_ID="+event.SessionID,
			"TCV_PROJECT="+event.Project,
			"TCV_PROJECT_PATH="+event.ProjectPath,
			"TCV_AGENT_TYPE="+event.AgentType,
			"TCV_CONTAINER_NAME="+event.ContainerName,
			"TCV_STATUS="+event.Status,
			"TCV_GIT_BRANCH="+event.GitBranch,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: hook %q failed: %v\n", event.Event, err)
			if len(output) > 0 {
				fmt.Fprintf(os.Stderr, "  output: %s\n", strings.TrimSpace(string(output)))
			}
		}
	}()
}

// containerRuntime caches the detected container runtime
var containerRuntime string

// getContainerRuntime detects and returns the available container runtime (podman or docker)
// Priority: TCV_CONTAINER_RUNTIME env var > podman > docker
func getContainerRuntime() string {
	if containerRuntime != "" {
		return containerRuntime
	}

	// Check env var override first
	if v := os.Getenv("TCV_CONTAINER_RUNTIME"); v != "" {
		containerRuntime = v
		return containerRuntime
	}

	// Prefer podman, fall back to docker
	if _, err := exec.LookPath("podman"); err == nil {
		containerRuntime = "podman"
		return containerRuntime
	}

	if _, err := exec.LookPath("docker"); err == nil {
		containerRuntime = "docker"
		return containerRuntime
	}

	// Default to podman (will fail with clear error if not installed)
	containerRuntime = "podman"
	return containerRuntime
}

// getHostGateway returns the hostname for reaching the host from within a container
// podman uses host.containers.internal, docker uses host.docker.internal
func getHostGateway() string {
	if getContainerRuntime() == "docker" {
		return "host.docker.internal"
	}
	return "host.containers.internal"
}

// containerSocketPath caches the detected socket path
var containerSocketPath string

// detectContainerSocket finds the Podman/Docker socket path
// Priority: CONTAINER_HOST env var > DOCKER_HOST env var > auto-detect
func detectContainerSocket() string {
	if containerSocketPath != "" {
		return containerSocketPath
	}

	// Check explicit env vars first
	if v := os.Getenv("CONTAINER_HOST"); v != "" {
		containerSocketPath = v
		return containerSocketPath
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		containerSocketPath = v
		return containerSocketPath
	}

	// Auto-detect Podman socket
	if getContainerRuntime() == "podman" {
		// Try XDG_RUNTIME_DIR first (standard location)
		if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
			socketPath := filepath.Join(xdgRuntime, "podman", "podman.sock")
			if _, err := os.Stat(socketPath); err == nil {
				containerSocketPath = "unix://" + socketPath
				return containerSocketPath
			}
		}

		// Try /run/user/<uid>/podman/podman.sock
		currentUser, err := user.Current()
		if err == nil {
			socketPath := filepath.Join("/run/user", currentUser.Uid, "podman", "podman.sock")
			if _, err := os.Stat(socketPath); err == nil {
				containerSocketPath = "unix://" + socketPath
				return containerSocketPath
			}
		}

		// Try rootful podman socket
		rootSocket := "/run/podman/podman.sock"
		if _, err := os.Stat(rootSocket); err == nil {
			containerSocketPath = "unix://" + rootSocket
			return containerSocketPath
		}
	}

	// For Docker, check common socket locations
	if getContainerRuntime() == "docker" {
		dockerSocket := "/var/run/docker.sock"
		if _, err := os.Stat(dockerSocket); err == nil {
			containerSocketPath = "unix://" + dockerSocket
			return containerSocketPath
		}
	}

	// Return empty string if no socket found (let podman/docker use defaults)
	return ""
}

// getContainerCmdEnv returns the environment variables for container commands
// with socket path set if detected
func getContainerCmdEnv() []string {
	env := os.Environ()
	socket := detectContainerSocket()
	if socket != "" {
		// Check if CONTAINER_HOST is already set in env
		hasContainerHost := false
		for _, e := range env {
			if strings.HasPrefix(e, "CONTAINER_HOST=") {
				hasContainerHost = true
				break
			}
		}
		if !hasContainerHost {
			env = append(env, "CONTAINER_HOST="+socket)
		}
	}
	return env
}

type resultRecord struct {
	Timestamp     time.Time              `json:"ts"`
	Type          string                 `json:"type"`
	Action        string                 `json:"action"`
	Project       string                 `json:"project,omitempty"`
	ContainerName string                 `json:"container_name,omitempty"`
	Image         string                 `json:"image,omitempty"`
	PolicyFile    string                 `json:"policy_file,omitempty"`
	Status        string                 `json:"status"`
	Message       string                 `json:"message,omitempty"`
	Details       map[string]interface{} `json:"details,omitempty"`
}

type resourceConfig struct {
	Memory    string `json:"memory,omitempty"`     // e.g., "4g"
	CPUs      string `json:"cpus,omitempty"`       // e.g., "2"
	PidsLimit string `json:"pids_limit,omitempty"` // e.g., "512"
}

type policyConfig struct {
	ProjectName  string                    `json:"project_name,omitempty"`
	ImageType    string                    `json:"image_type,omitempty"`    // Deprecated: use Sessions[].Image
	Resources    resourceConfig            `json:"resources,omitempty"`
	Agents       map[string]agentConfig    `json:"agents,omitempty"`        // Deprecated: use Sessions
	Sessions     map[string]sessionConfig  `json:"sessions,omitempty"`      // New: session type configs
	Mounts       []mountEntry              `json:"mounts,omitempty"`
	LocalDomains []string                  `json:"local_domains,omitempty"`
	LocalPorts   []int                     `json:"local_ports,omitempty"`
	Hooks        *hooksConfig              `json:"hooks,omitempty"`
}

type agentConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Default bool     `json:"default,omitempty"`
}

// sessionConfig defines a session type with its own container image
type sessionConfig struct {
	Image   string   `json:"image"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Default bool     `json:"default,omitempty"`
}

type mountEntry struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Mode        string `json:"mode,omitempty"` // "rw" or "ro", defaults to "rw"
}

type proxyResult struct {
	Status string
	Output string
}

type containerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	ID     string `json:"id"`
}

// sessionMeta matches the API's Session struct for interoperability
type sessionMeta struct {
	ID            string    `json:"id"`
	Project       string    `json:"project"`
	ProjectPath   string    `json:"projectPath,omitempty"`
	Name          string    `json:"name"`
	AgentType     string    `json:"agentType"`
	Cmd           string    `json:"cmd"`
	Status        string    `json:"status"`
	ContainerName string    `json:"containerName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	LastActivity  time.Time `json:"lastActivity"`
}

const (
	tcvLocalDir = ".tcv" // project-local storage directory
	statusPending   = "pending"
	statusRunning   = "running"
	statusStopped   = "stopped"
	statusFailed    = "failed"
	statusFinished  = "finished"
)

// loadBaselineLocalDomains loads local_domains from the baseline policy file.
// These domains (like MCP server hostnames) need --add-host entries in containers.
func loadBaselineLocalDomains() []string {
	tcvRoot := os.Getenv("TCV_ROOT")
	if tcvRoot == "" {
		tcvRoot = "/usr/local/share/tcv"
	}
	baselineFile := filepath.Join(tcvRoot, "images", "tcv-egress", "baseline-policy.json")

	content, err := os.ReadFile(baselineFile)
	if err != nil {
		// Try relative path from current binary location
		execPath, _ := os.Executable()
		if execPath != "" {
			altPath := filepath.Join(filepath.Dir(execPath), "..", "images", "tcv-egress", "baseline-policy.json")
			content, err = os.ReadFile(altPath)
		}
		if err != nil {
			return nil
		}
	}

	var baseline struct {
		LocalDomains []string `json:"local_domains"`
	}
	if err := json.Unmarshal(content, &baseline); err != nil {
		return nil
	}
	return baseline.LocalDomains
}

// agentMountConfig defines how an agent's config directory should be mounted
// The entire config dir is mounted read-write, with per-project history mounts overlaid on top
type agentMountConfig struct {
	Command       string   `json:"command"`
	Args          []string `json:"args,omitempty"`
	ConfigDir     string   `json:"config_dir"`     // e.g., ".claude" - mounted rw from host
	ContainerPath string   `json:"container_path"` // e.g., "/home/agent/.claude"
	HistoryMounts []string `json:"history_mounts"` // files/dirs to mount read-write per-project (overlays config dir)
	GitName       string   `json:"git_name,omitempty"`
	GitEmail      string   `json:"git_email,omitempty"`
}

// toolConfig defines a CLI tool to be mounted into containers
type toolConfig struct {
	Name          string `json:"name"`           // tool name (for logging)
	HostPath      string `json:"host_path"`      // path on host (supports ~)
	ContainerPath string `json:"container_path"` // path in container
}

// agentsConfig is the root config structure for agents.json
type agentsConfig struct {
	Comment string                      `json:"comment,omitempty"`
	Tools   []toolConfig                `json:"tools,omitempty"`
	Agents  map[string]agentMountConfig `json:"agents"`
}

// loadAgentsConfig loads agent mount configurations from config files
// Priority: project .tcv.json > user ~/.config/tcv/agents.json > global /usr/local/share/tcv/config/agents.json
func loadAgentsConfig() (map[string]agentMountConfig, error) {
	agents := make(map[string]agentMountConfig)

	// Load global config first (defaults)
	tcvRoot := os.Getenv("TCV_ROOT")
	if tcvRoot == "" {
		tcvRoot = "/usr/local/share/tcv"
	}
	globalConfig := filepath.Join(tcvRoot, "config", "agents.json")
	if err := loadAgentsFromFile(globalConfig, agents, nil); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load global agents config: %w", err)
	}

	// Load user config (overrides global)
	userConfigDir := os.Getenv("XDG_CONFIG_HOME")
	if userConfigDir == "" {
		home, _ := os.UserHomeDir()
		userConfigDir = filepath.Join(home, ".config")
	}
	userConfig := filepath.Join(userConfigDir, "tcv", "agents.json")
	if err := loadAgentsFromFile(userConfig, agents, nil); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load user agents config: %w", err)
	}

	return agents, nil
}

// loadToolsConfig loads tool configurations from config files
// Priority: user ~/.config/tcv/agents.json > global config/agents.json
func loadToolsConfig() ([]toolConfig, error) {
	var tools []toolConfig
	seenTools := make(map[string]bool) // dedupe by name

	// Load global config first (defaults)
	tcvRoot := os.Getenv("TCV_ROOT")
	if tcvRoot == "" {
		tcvRoot = "/usr/local/share/tcv"
	}
	globalConfig := filepath.Join(tcvRoot, "config", "agents.json")
	if err := loadAgentsFromFile(globalConfig, nil, &tools); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load global tools config: %w", err)
	}
	for _, t := range tools {
		seenTools[t.Name] = true
	}

	// Load user config (adds to global)
	userConfigDir := os.Getenv("XDG_CONFIG_HOME")
	if userConfigDir == "" {
		home, _ := os.UserHomeDir()
		userConfigDir = filepath.Join(home, ".config")
	}
	userConfig := filepath.Join(userConfigDir, "tcv", "agents.json")
	var userTools []toolConfig
	if err := loadAgentsFromFile(userConfig, nil, &userTools); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load user tools config: %w", err)
	}
	// Add user tools, overriding global ones with same name
	for _, t := range userTools {
		if seenTools[t.Name] {
			// Replace existing tool with same name
			for i, existing := range tools {
				if existing.Name == t.Name {
					tools[i] = t
					break
				}
			}
		} else {
			tools = append(tools, t)
			seenTools[t.Name] = true
		}
	}

	return tools, nil
}

// loadAgentsFromFile loads agents and tools from a single config file
func loadAgentsFromFile(path string, agents map[string]agentMountConfig, tools *[]toolConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var config agentsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse %s: %w", path, err)
	}

	// Merge agents (later configs override earlier ones)
	if agents != nil {
		for name, agent := range config.Agents {
			agents[name] = agent
		}
	}

	// Append tools
	if tools != nil && config.Tools != nil {
		*tools = append(*tools, config.Tools...)
	}

	return nil
}

func expandHomePath(path, homeDir string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(homeDir, path[2:])
	}
	return path
}

// generateUUID creates a random UUID v4
func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// getProjectSessionsDir returns the project-local sessions directory
func getProjectSessionsDir(projectDir string) string {
	return filepath.Join(projectDir, tcvLocalDir, "sessions")
}

// getProjectHistoryDir returns the project directory for agent history
// Agent configs (e.g., .claude/, .codex/) live directly in the project root
func getProjectHistoryDir(projectDir string) string {
	return projectDir
}

// createSessionDir creates the session directory within the project and returns the path
func createSessionDir(projectDir, sessionID string) (string, error) {
	sessionDir := filepath.Join(getProjectSessionsDir(projectDir), sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create session dir: %w", err)
	}
	// Create empty log files
	for _, name := range []string{"output.log", "stdout.log", "stderr.log", "events.jsonl"} {
		path := filepath.Join(sessionDir, name)
		if _, err := os.Create(path); err != nil {
			return "", fmt.Errorf("failed to create %s: %w", name, err)
		}
	}
	return sessionDir, nil
}

// writeSessionMeta writes the session metadata to meta.json
func writeSessionMeta(sessionDir string, meta sessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sessionDir, "meta.json"), data, 0644)
}

// updateSessionStatus updates just the status and timestamps in meta.json
// sessionDir is the full path to the session directory
func updateSessionStatus(sessionDir, status string) error {
	metaPath := filepath.Join(sessionDir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var meta sessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	meta.Status = status
	meta.UpdatedAt = time.Now().UTC()
	meta.LastActivity = time.Now().UTC()
	return writeSessionMeta(sessionDir, meta)
}

// imageInfo represents an available container image
type imageInfo struct {
	Name    string
	Path    string
	Type    string // "core" or "custom"
	Context string // build context path (empty = use Path)
}

type imageBuildConfig struct {
	Context string `json:"context"` // "repo-root" or path relative to image dir
}

// listAvailableImages scans the images directories and returns available images
func listAvailableImages() ([]imageInfo, error) {
	infraDir := getInfraDir()
	if infraDir == "" {
		return nil, fmt.Errorf("cannot find tcv directory. Set TCV_ROOT environment variable")
	}

	imagesDir := filepath.Join(infraDir, "images")
	customDir := filepath.Join(infraDir, "images-custom")

	var images []imageInfo

	// Helper to load build config and determine context
	loadImageInfo := func(name, path, imgType string) imageInfo {
		img := imageInfo{
			Name:    name,
			Path:    path,
			Type:    imgType,
			Context: path, // default: use image directory as context
		}
		// Check for build.json config
		buildConfigPath := filepath.Join(path, "build.json")
		if data, err := os.ReadFile(buildConfigPath); err == nil {
			var cfg imageBuildConfig
			if json.Unmarshal(data, &cfg) == nil && cfg.Context != "" {
				if cfg.Context == "repo-root" {
					img.Context = infraDir
				} else {
					img.Context = filepath.Join(path, cfg.Context)
				}
			}
		}
		return img
	}

	// Scan core images
	if entries, err := os.ReadDir(imagesDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				containerfile := filepath.Join(imagesDir, entry.Name(), "Containerfile")
				if fileExists(containerfile) {
					images = append(images, loadImageInfo(
						entry.Name(),
						filepath.Join(imagesDir, entry.Name()),
						"core",
					))
				}
			}
		}
	}

	// Scan custom images
	if entries, err := os.ReadDir(customDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				containerfile := filepath.Join(customDir, entry.Name(), "Containerfile")
				if fileExists(containerfile) {
					images = append(images, loadImageInfo(
						entry.Name(),
						filepath.Join(customDir, entry.Name()),
						"custom",
					))
				}
			}
		}
	}

	return images, nil
}

// promptImageSelection displays available images and prompts user to select one
// Returns the selected image name or empty string if cancelled
func promptImageSelection(detected string) (string, error) {
	images, err := listAvailableImages()
	if err != nil {
		return "", err
	}

	if len(images) == 0 {
		return "", fmt.Errorf("no images found")
	}

	// Separate into agent images (for projects) and infrastructure images
	var agentImages []imageInfo
	for _, img := range images {
		if strings.HasPrefix(img.Name, "agent-") {
			agentImages = append(agentImages, img)
		}
	}

	if len(agentImages) == 0 {
		return "", fmt.Errorf("no agent images found")
	}

	fmt.Println("\nAvailable agent images:")
	fmt.Println()

	// Find the index of the detected image for highlighting
	detectedIdx := -1
	for i, img := range agentImages {
		marker := "  "
		if img.Name == detected {
			marker = "* "
			detectedIdx = i
		}
		fmt.Printf("%s%2d) %s", marker, i+1, img.Name)
		if img.Type == "custom" {
			fmt.Print(" (custom)")
		}
		fmt.Println()
	}

	fmt.Println()
	if detectedIdx >= 0 {
		fmt.Printf("Detected: %s (marked with *)\n", detected)
	}

	// Prompt for selection
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\nSelect image number (or press Enter for detected): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}

		input = strings.TrimSpace(input)

		// Empty input = use detected
		if input == "" {
			if detected != "" {
				return detected, nil
			}
			fmt.Println("No detected image. Please enter a number.")
			continue
		}

		// Parse number
		var idx int
		if _, err := fmt.Sscanf(input, "%d", &idx); err != nil || idx < 1 || idx > len(agentImages) {
			fmt.Printf("Invalid selection. Enter a number between 1 and %d.\n", len(agentImages))
			continue
		}

		return agentImages[idx-1].Name, nil
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return errors.New("missing command")
	}

	switch args[0] {
	// Session-type commands (preferred)
	case "claude":
		return sessionTypeCommand("claude", args[1:])
	case "codex":
		return sessionTypeCommand("codex", args[1:])

	// Legacy start command (retired)
	case "start":
		return fmt.Errorf("'tcv start' has been retired. Use 'tcv claude', 'tcv codex', instead")
	case "stop":
		return stopCommand(args[1:])
	case "kill":
		return killCommand(args[1:])
	case "status":
		return statusCommand(args[1:])
	case "logs":
		return logsCommand(args[1:])
	case "attach":
		return attachCommand(args[1:])
	case "reconnect":
		return reconnectCommand(args[1:])
	case "init":
		return initCommand(args[1:])
	case "reload":
		return reloadCommand(args[1:])
	case "proxy":
		return runProxy(args[1:])
	case "build":
		return buildCommand(args[1:])
	case "baseline":
		return baselineCommand(args[1:])

	// Legacy commands (keep for backwards compatibility)
	case "session":
		return runSession(args[1:])

	case "version", "-v", "--version":
		fmt.Printf("tcv version %s\n", version)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

// Legacy session command handler for backwards compatibility
func runSession(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing session subcommand")
	}

	switch args[0] {
	case "start":
		return fmt.Errorf("'tcv session start' has been retired. Use 'tcv claude', 'tcv codex', instead")
	case "stop":
		return stopCommand(args[1:])
	case "kill":
		return killCommand(args[1:])
	case "status":
		return statusCommand(args[1:])
	default:
		return fmt.Errorf("unknown session subcommand: %s", args[0])
	}
}

// sessionTypeCommand handles commands like "tcv claude [project-dir]"
func sessionTypeCommand(sessionType string, args []string) error {
	fs := flag.NewFlagSet(sessionType, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		projectDir     string
		projectName    string
		envFile        string
		policyOverride string
		timeout        time.Duration
		headless       bool
		sessionLogDir  string
	)

	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")
	fs.StringVar(&projectName, "project-name", "", "Logical project name (defaults to directory name)")
	fs.StringVar(&projectName, "n", "", "Logical project name (shorthand)")
	fs.StringVar(&envFile, "env-file", "", "Path to KEY=VALUE env file merged into the shell environment")
	fs.StringVar(&policyOverride, "policy", "", "Override policy file path")
	fs.DurationVar(&timeout, "timeout", 0, "Timeout for the session start command (e.g. 10m)")
	fs.BoolVar(&headless, "headless", false, "Run in headless mode (auto-detected if no TTY)")
	fs.StringVar(&sessionLogDir, "session-log-dir", "", "Directory for session output logs (headless mode)")
	var noProxy bool
	fs.BoolVar(&noProxy, "no-proxy", false, "Bypass the egress proxy (direct internet access)")
	var batch bool
	var batchPrompt string
	fs.BoolVar(&batch, "batch", false, "Run agent with a prompt and exit when done (non-interactive)")
	fs.StringVar(&batchPrompt, "prompt", "", "Prompt string for batch mode (requires --batch)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Positional arg is project directory
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	// Validate batch flags
	if batchPrompt != "" && !batch {
		return fmt.Errorf("--prompt requires --batch flag")
	}
	if batch {
		// Batch mode implies headless
		headless = true
	}

	return startSessionWithType(sessionType, projectDir, projectName, "", envFile,
		policyOverride, timeout, headless, sessionLogDir, noProxy, batch, batchPrompt)
}

// startSessionWithType is the unified session start implementation.
// sessionType specifies which session config to use (e.g., "claude", "codex").
// cmdOverride allows overriding the command from the session config.
func startSessionWithType(sessionType, projectDir, projectName, cmdOverride, envFile,
	policyOverride string, timeout time.Duration, headless bool, sessionLogDir string, noProxy bool, batch bool, batchPrompt string) error {

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	// Clean up session directories older than 7 days
	cleanupOldSessions(absDir)

	policyPath := policyOverride
	if policyPath == "" {
		policyPath = filepath.Join(absDir, ".tcv.json")
	}
	if !fileExists(policyPath) {
		return fmt.Errorf("policy file not found: %s\nRun 'tcv init' to create one", policyPath)
	}

	policy, err := readPolicy(policyPath)
	if err != nil {
		return err
	}

	hooks := loadHooksConfig(policy)

	// Merge baseline policy's local_domains (for MCP servers, etc.)
	baselineDomains := loadBaselineLocalDomains()
	for _, domain := range baselineDomains {
		// Add if not already present
		found := false
		for _, existing := range policy.LocalDomains {
			if existing == domain {
				found = true
				break
			}
		}
		if !found {
			policy.LocalDomains = append(policy.LocalDomains, domain)
		}
	}

	// Resolve project name: CLI flag > policy file > directory name
	if projectName == "" {
		if policy.ProjectName != "" {
			projectName = policy.ProjectName
		} else {
			projectName = filepath.Base(absDir)
		}
	}

	// Check for existing session before proceeding
	if err := checkExistingSession(absDir, projectName); err != nil {
		return err
	}

	// Resolve session config
	sessionCfg, resolvedSessionType, err := resolveSessionConfig(policy, sessionType)
	if err != nil {
		return err
	}

	// Determine command: override > session config
	agentCmd := cmdOverride
	if agentCmd == "" {
		agentCmd = sessionCfg.Command
		if len(sessionCfg.Args) > 0 {
			agentCmd = agentCmd + " " + strings.Join(sessionCfg.Args, " ")
		}
	}

	// Get image from session config
	imageType := sessionCfg.Image
	if imageType == "" {
		imageType = "agent-php-vcs"
	}
	// Add version if not specified
	if !strings.Contains(imageType, ":") {
		imageType = imageType + ":1.0"
	}
	fullImageName := resolveImageName(imageType)

	// Get git branch for terminal title
	gitBranch := getGitBranch(absDir)

	extraEnv, err := loadEnvFile(envFile)
	if err != nil {
		return err
	}

	// Ensure proxy is running (unless bypassed)
	var proxyRes proxyResult
	if noProxy {
		proxyRes = proxyResult{Status: "bypassed"}
		fmt.Fprintf(os.Stderr, "note: egress proxy bypassed (--no-proxy)\n")
	} else {
		var err error
		proxyRes, err = ensureProxy(context.Background())
		if err != nil {
			return fmt.Errorf("proxy ensure failed: %w", err)
		}

		// Add policy to proxy
		if err := addPolicyToProxy(policyPath, projectName); err != nil {
			// Log but don't fail - proxy might still work
			fmt.Fprintf(os.Stderr, "warning: failed to add policy to proxy: %v\n", err)
		}
	}

	// Auto-detect headless mode if stdin is not a TTY
	if !headless && !term.IsTerminal(int(os.Stdin.Fd())) {
		headless = true
	}

	// Determine if we're using an API-provided session directory or creating our own
	var sessionDir string
	var sessionID string
	apiProvidedSession := sessionLogDir != ""

	if apiProvidedSession {
		// API already created the session - use provided directory
		sessionDir = sessionLogDir
		// Extract session ID from path (last component)
		sessionID = filepath.Base(sessionLogDir)
	} else {
		// CLI-initiated session - create our own session directory
		sessionID = generateUUID()
		var err error
		sessionDir, err = createSessionDir(absDir, sessionID)
		if err != nil {
			return fmt.Errorf("failed to create session directory: %w", err)
		}
	}

	containerName := fmt.Sprintf("agent-%s-%d", projectName, os.Getpid())

	// Add --session-id to Claude command so Claude uses TCV's session ID
	// This makes the JSONL predictably at .claude/projects/{encoded-path}/{sessionID}.jsonl
	if resolvedSessionType == "claude" {
		agentCmd = agentCmd + " --session-id " + sessionID
	}

	// Batch mode: pass prompt via environment variable to avoid shell injection
	var batchPromptEnv string
	if batch && resolvedSessionType == "claude" {
		prompt := batchPrompt
		if prompt == "" {
			prompt = defaultBatchPrompt
		}
		batchPromptEnv = prompt
		agentCmd = agentCmd + " -p \"$TCV_BATCH_PROMPT\""
	}

	// Use resolved session type as agent type
	agentType := resolvedSessionType

	// Session name for CLI-created sessions
	sessionName := fmt.Sprintf("CLI session - %s", time.Now().UTC().Format("2006-01-02 15:04"))

	// Only write session metadata when CLI creates the session (not API)
	if !apiProvidedSession {
		now := time.Now().UTC()
		meta := sessionMeta{
			ID:            sessionID,
			Project:       projectName,
			ProjectPath:   absDir,
			Name:          sessionName,
			AgentType:     agentType,
			Cmd:           agentCmd,
			Status:        statusPending,
			ContainerName: containerName,
			CreatedAt:     now,
			UpdatedAt:     now,
			LastActivity:  now,
		}
		if err := writeSessionMeta(sessionDir, meta); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write session meta: %v\n", err)
		}
	}

	// Write container ID file (legacy, for backwards compatibility)
	containerIDPath := filepath.Join(absDir, ".container_id")
	if err := os.WriteFile(containerIDPath, []byte(containerName), 0644); err != nil {
		return fmt.Errorf("failed to write container ID: %w", err)
	}

	// Also write session ID for reference
	sessionIDPath := filepath.Join(absDir, ".session_id")
	_ = os.WriteFile(sessionIDPath, []byte(sessionID), 0644)

	// Get current user info
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}
	uid := currentUser.Uid
	gid := currentUser.Gid
	homeDir := currentUser.HomeDir

	// Load agent mount configurations
	agentConfigs, err := loadAgentsConfig()
	if err != nil {
		return fmt.Errorf("failed to load agents config: %w", err)
	}

	// Load tool configurations
	toolConfigs, err := loadToolsConfig()
	if err != nil {
		return fmt.Errorf("failed to load tools config: %w", err)
	}

	var activeAgentConfig agentMountConfig
	hasActiveAgentConfig := false
	if cfg, ok := agentConfigs[agentType]; ok {
		activeAgentConfig = cfg
		hasActiveAgentConfig = true
	}

	// Use project-local history directory
	projectHistoryDir := getProjectHistoryDir(absDir)

	// Create agent home directory
	agentHomeDir := filepath.Join(homeDir, ".agent-home")
	if err := os.MkdirAll(agentHomeDir, 0755); err != nil {
		return fmt.Errorf("failed to create agent home directory: %w", err)
	}

	// Pre-create agent config directories inside agent-home so they exist
	// with correct ownership before the container runtime overlays individual mounts.
	for _, agentCfg := range agentConfigs {
		if agentCfg.ConfigDir != "" {
			agentConfigDir := filepath.Join(agentHomeDir, agentCfg.ConfigDir)
			if err := os.MkdirAll(agentConfigDir, 0755); err != nil {
				return fmt.Errorf("failed to create agent config dir %s: %w", agentConfigDir, err)
			}
		}
	}

	// Use session directory for logs (only set if not provided by API)
	if !apiProvidedSession {
		sessionLogDir = sessionDir
	}

	// Build volume mounts - start with base mounts
	projectMountPath := absDir
	volumeArgs := []string{
		"-v", absDir + ":" + projectMountPath + ":rw",
		"-v", policyPath + ":/policy/.tcv.json:ro",
		"-v", agentHomeDir + ":/home/agent:rw",
	}

	// Add agent-specific mounts from config
	for agentName, agentCfg := range agentConfigs {
		if agentCfg.ConfigDir == "" || agentCfg.ContainerPath == "" {
			continue
		}

		hostConfigDir := filepath.Join(homeDir, agentCfg.ConfigDir)
		if !fileExists(hostConfigDir) {
			continue
		}

		projectAgentHistoryDir := filepath.Join(projectHistoryDir, agentCfg.ConfigDir)
		if err := os.MkdirAll(projectAgentHistoryDir, 0755); err != nil {
			return fmt.Errorf("failed to create project history dir %s for %s: %w", projectAgentHistoryDir, agentName, err)
		}

		volumeArgs = append(volumeArgs, "-v", hostConfigDir+":"+agentCfg.ContainerPath+":rw")

		for _, historyMount := range agentCfg.HistoryMounts {
			hostPath := filepath.Join(projectAgentHistoryDir, historyMount)
			containerPath := filepath.Join(agentCfg.ContainerPath, historyMount)
			containerPath = strings.TrimSuffix(containerPath, "/")

			if strings.HasSuffix(historyMount, "/") {
				if err := os.MkdirAll(hostPath, 0755); err != nil {
					return fmt.Errorf("failed to create history dir %s for %s: %w", hostPath, agentName, err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(hostPath), 0755); err != nil {
					return fmt.Errorf("failed to create history parent dir for %s: %w", agentName, err)
				}
				if !fileExists(hostPath) {
					if err := os.WriteFile(hostPath, []byte{}, 0644); err != nil {
						return fmt.Errorf("failed to create history file %s for %s: %w", hostPath, agentName, err)
					}
				}
			}

			volumeArgs = append(volumeArgs, "-v", hostPath+":"+containerPath+":rw")
		}
	}

	// Add session log mount
	volumeArgs = append(volumeArgs, "-v", sessionLogDir+":/session:rw")

	// Mount host's .claude.json config if it exists
	claudeJsonPath := filepath.Join(homeDir, ".claude.json")
	if fileExists(claudeJsonPath) {
		volumeArgs = append(volumeArgs, "-v", claudeJsonPath+":/home/agent/.claude.json:rw")
	}

	// NOTE: .credentials.json is NOT mounted separately - it's included in the
	// ~/.claude directory mount (via agents.json config). Mounting it as a separate
	// file prevents atomic rename operations needed for token refresh.

	// Add optional mounts
	notifyScript := filepath.Join(homeDir, "bin/notify-container.sh")
	if fileExists(notifyScript) {
		volumeArgs = append(volumeArgs, "-v", notifyScript+":/home/agent/bin/notify.sh:ro")
	}
	binEnvFile := filepath.Join(homeDir, "bin/.env")
	if fileExists(binEnvFile) {
		volumeArgs = append(volumeArgs, "-v", binEnvFile+":/home/agent/bin/.env:ro")
	}

	// Add configured tools
	for _, tool := range toolConfigs {
		hostPath := expandHomePath(tool.HostPath, homeDir)
		if fileExists(hostPath) {
			volumeArgs = append(volumeArgs, "-v", hostPath+":"+tool.ContainerPath+":ro")
		}
	}

	// Add extra mounts from policy
	for _, mount := range policy.Mounts {
		if mount.Source != "" && mount.Destination != "" {
			if fileExists(mount.Source) {
				mode := mount.Mode
				if mode == "" {
					mode = "rw"
				}
				volumeArgs = append(volumeArgs, "-v", mount.Source+":"+mount.Destination+":"+mode)
			}
		}
	}

	// Get git config for author info
	gitAuthorName := getGitConfig("user.name", "Claude Code")
	gitAuthorEmail := getGitConfig("user.email", "claude@agent.local")
	if hasActiveAgentConfig {
		if activeAgentConfig.GitName != "" {
			gitAuthorName = activeAgentConfig.GitName
		}
		if activeAgentConfig.GitEmail != "" {
			gitAuthorEmail = activeAgentConfig.GitEmail
		}
	}

	// Build environment variables
	envArgs := []string{
		"-e", "HOME=/home/agent",
	}
	if !noProxy {
		proxyURL := fmt.Sprintf("http://%s:%s", getHostGateway(), getProxyPort())
		envArgs = append(envArgs, "-e", "HTTP_PROXY="+proxyURL, "-e", "HTTPS_PROXY="+proxyURL)
	}
	envArgs = append(envArgs,
		"-e", "ANTHROPIC_API_KEY=" + os.Getenv("ANTHROPIC_API_KEY"),
		"-e", "OPENAI_API_KEY=" + os.Getenv("OPENAI_API_KEY"),
		"-e", "GIT_AUTHOR_NAME=" + gitAuthorName,
		"-e", "GIT_AUTHOR_EMAIL=" + gitAuthorEmail,
		"-e", "GIT_COMMITTER_NAME=" + gitAuthorName,
		"-e", "GIT_COMMITTER_EMAIL=" + gitAuthorEmail,
		"-e", "PATH=/opt/agent-cli/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/bin:/sbin:/bin",
		"-e", "TERM=xterm-256color",
		"-e", "CLICOLOR_FORCE=1",
		"-e", "TCV_PROJECT=" + projectName,
		"-e", "TCV_PROJECT_PATH=" + absDir,
		"-e", "TCV_BRANCH=" + gitBranch,
	)

	// Add batch prompt as env var (avoids shell injection from prompt content)
	if batchPromptEnv != "" {
		envArgs = append(envArgs, "-e", "TCV_BATCH_PROMPT="+batchPromptEnv)
	}

	// Add extra env vars
	for _, e := range extraEnv {
		envArgs = append(envArgs, "-e", e)
	}

	// Build the container run command
	podmanArgs := []string{"run", "--rm"}

	if headless {
		podmanArgs = append(podmanArgs, "-d") // detached mode
	} else {
		podmanArgs = append(podmanArgs, "-it") // interactive with TTY
	}

	podmanArgs = append(podmanArgs,
		"--name", containerName,
		"--network", "agent-net",
		"--userns=keep-id",
		"--user", uid+":"+gid,
	)

	// Add --add-host entries for local domains
	for _, domain := range policy.LocalDomains {
		podmanArgs = append(podmanArgs, "--add-host", domain+":host-gateway")
	}

	podmanArgs = append(podmanArgs, volumeArgs...)
	podmanArgs = append(podmanArgs,
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=1g",
		"--workdir", projectMountPath,
	)
	podmanArgs = append(podmanArgs, envArgs...)

	// Resource limits - use config values or defaults
	pidsLimit := policy.Resources.PidsLimit
	if pidsLimit == "" {
		pidsLimit = "128"
	}
	memory := policy.Resources.Memory
	if memory == "" {
		memory = "1g"
	}
	cpus := policy.Resources.CPUs
	if cpus == "" {
		cpus = "1"
	}
	podmanArgs = append(podmanArgs,
		"--pids-limit", pidsLimit,
		"--memory", memory,
		"--cpus", cpus,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		fullImageName,
	)

	// Build terminal title: "project @ branch"
	terminalTitle := projectName
	if gitBranch != "" {
		terminalTitle = fmt.Sprintf("%s @ %s", projectName, gitBranch)
	}
	setTitleCmd := fmt.Sprintf(`printf '\033]0;%s\007'`, terminalTitle)

	// Default tmux dimensions — matches UI's .env TERMINAL_COLS/ROWS.
	// The UI will send a resize via WebSocket on connect, but setting this
	// upfront prevents the initial output from being rendered at 80x24.
	initialCols := 111
	initialRows := 61

	if batch {
		// Batch mode: run agent directly (no tmux), exit when done
		// - Output captured to log file via tee
		// - Container exits when agent process exits
		// - No interactive session, no tmux overhead
		wrapperCmd := fmt.Sprintf(
			`/usr/local/bin/entrypoint.sh %s 2>&1 | tee /session/output.log; exit ${PIPESTATUS[0]}`,
			agentCmd,
		)
		podmanArgs = append(podmanArgs, "/bin/bash", "-c", wrapperCmd)
	} else if headless {
		// Headless mode: tmux session with pipe-pane for output capture
		// - Agent runs in tmux "agent-session" (matches WebSocket input target)
		// - Output piped to log file for UI streaming (trailing spaces stripped)
		// - Mouse mode disabled for natural scroll when attached
		// - If agent crashes, container stays alive for debugging
		// - User can attach with "tcv attach <project>"
		tmuxSession := "agent-session"
		// tmux config: mouse on with proper wheel bindings for scrollback
		tmuxConf := `set -g mouse on
set -g history-limit 50000
bind -n WheelUpPane if-shell -F -t = "#{mouse_any_flag}" "send-keys -M" "copy-mode -e; send-keys -M"
bind -n WheelDownPane send-keys -M
`
		wrapperCmd := fmt.Sprintf(
			`cat > /tmp/tmux.conf << 'TMUXEOF'
%s
TMUXEOF
tmux -f /tmp/tmux.conf new-session -d -s %s -x %d -y %d 'bash -c "/usr/local/bin/entrypoint.sh %s; echo; echo \"=== Agent exited at $(date). ===\"; echo \"Attach with: tcv attach <project>\"; exec bash"' && `+
				`tmux pipe-pane -t %s -o 'sed "s/ *$//" >> /session/output.log' && `+
				`sleep infinity`,
			tmuxConf, tmuxSession, initialCols, initialRows, agentCmd, tmuxSession,
		)
		podmanArgs = append(podmanArgs, "/bin/bash", "-c", wrapperCmd)
	} else {
		// Attached mode: use tmux with pipe-pane for output capture, then attach
		// - Output is captured to /session/output.log for UI streaming
		// - User can type in terminal OR UI (via WebSocket send-keys)
		// - Mouse mode disabled for natural scroll behavior
		// - Container exits when user exits or detaches
		tmuxSession := "agent-session"
		// tmux config: mouse on with proper wheel bindings for scrollback
		tmuxConf := `set -g mouse on
set -g history-limit 50000
bind -n WheelUpPane if-shell -F -t = "#{mouse_any_flag}" "send-keys -M" "copy-mode -e; send-keys -M"
bind -n WheelDownPane send-keys -M
`
		wrapperCmd := fmt.Sprintf(
			`%s; cat > /tmp/tmux.conf << 'TMUXEOF'
%s
TMUXEOF
tmux -f /tmp/tmux.conf new-session -d -s %s -x %d -y %d 'bash -c "/usr/local/bin/entrypoint.sh %s; exec bash"' && `+
				`tmux pipe-pane -t %s -o 'sed "s/ *$//" >> /session/output.log' && `+
				`tmux attach -t %s`,
			setTitleCmd, tmuxConf, tmuxSession, initialCols, initialRows, agentCmd, tmuxSession, tmuxSession,
		)
		podmanArgs = append(podmanArgs, "/bin/bash", "-c", wrapperCmd)
	}

	// Execute container runtime
	var cmdCtx context.Context = context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	}

	startedAt := time.Now().UTC()
	cmd := exec.CommandContext(cmdCtx, getContainerRuntime(), podmanArgs...)
	cmd.Dir = absDir
	cmd.Env = getContainerCmdEnv()

	var outputBuf, stderrBuf bytes.Buffer
	if headless {
		cmd.Stdout = &outputBuf
		cmd.Stderr = io.MultiWriter(&outputBuf, &stderrBuf)
	} else {
		_ = updateSessionStatus(sessionDir, statusRunning)

		// Fire session.started hook BEFORE cmd.Run() blocks
		// (for attached sessions, cmd.Run() doesn't return until the user exits)
		if !apiProvidedSession {
			runHook(hooks.SessionStarted, sessionEvent{
				Event:         "session.started",
				SessionID:     sessionID,
				Project:       projectName,
				ProjectPath:   absDir,
				Name:          sessionName,
				AgentType:     agentType,
				Cmd:           agentCmd,
				Status:        statusRunning,
				ContainerName: containerName,
				GitBranch:     gitBranch,
				GitCommit:     getGitCommit(absDir),
				GitDirty:      isGitDirty(absDir),
				Timestamp:     time.Now().UTC().Format(time.RFC3339),
			})
		}

		cmd.Stdin = os.Stdin
		cmd.Stdout = io.MultiWriter(&outputBuf, os.Stdout)
		cmd.Stderr = io.MultiWriter(&outputBuf, &stderrBuf, os.Stderr)
	}

	runErr := cmd.Run()
	finishedAt := time.Now().UTC()

	details := map[string]interface{}{
		"exit_code":      exitCode(runErr),
		"proxy_state":    proxyRes.Status,
		"started_at":     startedAt.Format(time.RFC3339),
		"finished_at":    finishedAt.Format(time.RFC3339),
		"workdir":        absDir,
		"cmd":            agentCmd,
		"headless":       headless,
		"session_log":    filepath.Join(sessionLogDir, "output.log"),
		"container_name": containerName,
	}
	if headless {
		details["output"] = strings.TrimSpace(outputBuf.String())
		details["stderr"] = strings.TrimSpace(stderrBuf.String())
	}

	status := "completed"
	message := "session started"
	sessionStatus := statusFinished
	if runErr != nil {
		if headless {
			if isContainerRunning(containerName) {
				status = "running"
				sessionStatus = statusRunning
				message = "session started in headless mode"
				runErr = nil
			} else {
				status = "error"
				sessionStatus = statusFailed
				message = fmt.Sprintf("session start failed: %v", runErr)
			}
		} else {
			status = "error"
			sessionStatus = statusFailed
			message = fmt.Sprintf("session start failed: %v", runErr)
		}
	} else if headless {
		status = "running"
		sessionStatus = statusRunning
		message = "session started in headless mode"
	}

	_ = updateSessionStatus(sessionDir, sessionStatus)

	// Fire session.started hook for headless sessions (after we know the container status)
	// Note: Attached sessions fire the hook before cmd.Run() since it blocks until exit
	if !apiProvidedSession && headless {
		runHook(hooks.SessionStarted, sessionEvent{
			Event:         "session.started",
			SessionID:     sessionID,
			Project:       projectName,
			ProjectPath:   absDir,
			Name:          sessionName,
			AgentType:     agentType,
			Cmd:           agentCmd,
			Status:        sessionStatus,
			ContainerName: containerName,
			GitBranch:     gitBranch,
			GitCommit:     getGitCommit(absDir),
			GitDirty:      isGitDirty(absDir),
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
		})
	}

	// Fire session.stopped hook for attached sessions (cmd.Run() returned = session ended)
	// Headless sessions don't fire stopped here — they're still running
	if !apiProvidedSession && !headless {
		runHook(hooks.SessionStopped, sessionEvent{
			Event:         "session.stopped",
			SessionID:     sessionID,
			Project:       projectName,
			ProjectPath:   absDir,
			Name:          sessionName,
			AgentType:     agentType,
			Cmd:           agentCmd,
			Status:        sessionStatus,
			ContainerName: containerName,
			GitBranch:     gitBranch,
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
		})
	}

	record := resultRecord{
		Timestamp:     time.Now().UTC(),
		Type:          "session",
		Action:        "start",
		Project:       projectName,
		ContainerName: containerName,
		Image:         fullImageName,
		PolicyFile:    policyPath,
		Status:        status,
		Message:       message,
		Details:       details,
	}
	record.Details["session_id"] = sessionID

	if runErr != nil {
		_ = writeJSON(record)
		return runErr
	}

	return writeJSON(record)
}

func runProxy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing proxy subcommand (start, stop, status)")
	}

	switch args[0] {
	case "start", "ensure":
		return proxyEnsureCommand()
	case "stop":
		return proxyStopCommand()
	case "status":
		res, err := checkProxyStatus(context.Background())
		if err != nil {
			return writeJSON(resultRecord{
				Timestamp: time.Now().UTC(),
				Type:      "proxy",
				Action:    "status",
				Status:    "stopped",
				Message:   "proxy not running",
				Details: map[string]interface{}{
					"error": err.Error(),
				},
			})
		}
		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "proxy",
			Action:    "status",
			Status:    res.Status,
			Message:   "proxy status",
			Details: map[string]interface{}{
				"output": res.Output,
			},
		})
	default:
		return fmt.Errorf("unknown proxy subcommand: %s", args[0])
	}
}

func stopCommand(args []string) error {
	return stopOrKillCommand(args, false)
}

func killCommand(args []string) error {
	return stopOrKillCommand(args, true)
}

func stopOrKillCommand(args []string, forceKill bool) error {
	cmdName := "stop"
	if forceKill {
		cmdName = "kill"
	}

	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var projectDir string
	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	project := filepath.Base(absDir)
	ctx := context.Background()

	var stopped []string

	// First, try to read .container_id for exact container name
	containerIDPath := filepath.Join(absDir, ".container_id")
	if containerName, err := os.ReadFile(containerIDPath); err == nil {
		name := strings.TrimSpace(string(containerName))
		if name != "" {
			cmd := exec.CommandContext(ctx, getContainerRuntime(), cmdName, name)
			if err := cmd.Run(); err == nil {
				stopped = append(stopped, name)
			}
		}
	}

	// If no container was stopped via .container_id, fall back to pattern matching
	if len(stopped) == 0 {
		containers, _ := listProjectContainers(project, "")
		for _, c := range containers {
			cmd := exec.CommandContext(ctx, getContainerRuntime(), cmdName, c.Name)
			if err := cmd.Run(); err == nil {
				stopped = append(stopped, c.Name)
			}
		}
	}

	// Remove container ID file
	os.Remove(containerIDPath)

	// Update session status if session ID file exists
	sessionIDPath := filepath.Join(absDir, ".session_id")
	if sessionIDBytes, err := os.ReadFile(sessionIDPath); err == nil {
		sessionID := strings.TrimSpace(string(sessionIDBytes))
		sessionDir := filepath.Join(getProjectSessionsDir(absDir), sessionID)
		_ = updateSessionStatus(sessionDir, statusStopped)
		os.Remove(sessionIDPath)
	}

	// Remove policy from proxy
	removePolicyFromProxy(project)

	return writeJSON(resultRecord{
		Timestamp: time.Now().UTC(),
		Type:      "session",
		Action:    "stop",
		Project:   project,
		Status:    "stopped",
		Message:   "session stopped",
		Details: map[string]interface{}{
			"stopped_containers": stopped,
		},
	})
}

func statusCommand(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var projectDir string
	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	project := filepath.Base(absDir)
	containerPath := filepath.Join(absDir, ".container_id")
	containerName := ""
	if data, readErr := os.ReadFile(containerPath); readErr == nil {
		containerName = strings.TrimSpace(string(data))
	}

	containers, listErr := listProjectContainers(project, containerName)
	if listErr != nil {
		return listErr
	}

	state := "stopped"
	for _, c := range containers {
		if strings.HasPrefix(strings.ToLower(c.Status), "up") || strings.Contains(strings.ToLower(c.Status), "running") {
			state = "running"
			break
		}
		state = "exited"
	}

	detailContainers := make([]map[string]string, 0, len(containers))
	for _, c := range containers {
		detailContainers = append(detailContainers, map[string]string{
			"name":   c.Name,
			"status": c.Status,
			"id":     c.ID,
		})
	}

	return writeJSON(resultRecord{
		Timestamp: time.Now().UTC(),
		Type:      "session",
		Action:    "status",
		Project:   project,
		Status:    state,
		Message:   "session status",
		Details: map[string]interface{}{
			"containers": detailContainers,
		},
	})
}

// attachCommand connects to the tmux agent session inside a running container.
// This allows interactive debugging when an agent crashes but the container stays alive.
func attachCommand(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var projectDir string
	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	project := filepath.Base(absDir)

	// Try to read .container_id for exact container name
	containerIDPath := filepath.Join(absDir, ".container_id")
	containerName := ""
	if data, readErr := os.ReadFile(containerIDPath); readErr == nil {
		containerName = strings.TrimSpace(string(data))
	}

	// Find running container
	containers, listErr := listProjectContainers(project, containerName)
	if listErr != nil {
		return listErr
	}

	var runningContainer string
	for _, c := range containers {
		status := strings.ToLower(c.Status)
		if strings.HasPrefix(status, "up") || strings.Contains(status, "running") {
			runningContainer = c.Name
			break
		}
	}

	if runningContainer == "" {
		return fmt.Errorf("no running container found for project %s", project)
	}

	// Attach to the agent tmux session inside the container
	runtime := getContainerRuntime()
	cmd := exec.Command(runtime, "exec", "-it", runningContainer, "tmux", "attach-session", "-t", "agent-session")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Fprintf(os.Stderr, "Attaching to tmux session in %s...\n", runningContainer)
	fmt.Fprintf(os.Stderr, "Detach with: Ctrl-b d\n\n")

	return cmd.Run()
}

// reconnectCommand reconnects to a crashed session.
// If the container is still running, attaches to it.
// If the container died (e.g., terminal crash), restarts with the same agent type.
// Claude Code's history is preserved in .claude/ so /resume will work.
func reconnectCommand(args []string) error {
	fs := flag.NewFlagSet("reconnect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var projectDir string
	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	// Read session ID from .session_id
	sessionIDPath := filepath.Join(absDir, ".session_id")
	sessionIDBytes, err := os.ReadFile(sessionIDPath)
	if err != nil {
		return fmt.Errorf("no session to reconnect (no .session_id file in %s)", absDir)
	}
	sessionID := strings.TrimSpace(string(sessionIDBytes))

	// Read container name from .container_id
	containerIDPath := filepath.Join(absDir, ".container_id")
	containerName := ""
	if data, readErr := os.ReadFile(containerIDPath); readErr == nil {
		containerName = strings.TrimSpace(string(data))
	}

	// Check if container is still running
	if containerName != "" && isContainerRunning(containerName) {
		fmt.Fprintf(os.Stderr, "Container %s is still running. Attaching...\n", containerName)
		return attachToContainer(containerName)
	}

	// Container is not running - need to restart
	fmt.Fprintf(os.Stderr, "Container not running. Reading session metadata...\n")

	// Read session meta to get agent type
	metaPath := filepath.Join(getProjectSessionsDir(absDir), sessionID, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("cannot read session metadata: %w\nTry 'tcv claude' to start a fresh session", err)
	}

	var meta sessionMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("invalid session metadata: %w", err)
	}

	agentType := meta.AgentType
	if agentType == "" {
		agentType = "claude" // default
	}

	fmt.Fprintf(os.Stderr, "Restarting %s session (previous: %s)...\n", agentType, sessionID[:8])
	fmt.Fprintf(os.Stderr, "Claude history is in .claude/ - use /resume to continue.\n\n")

	// Clean up stale files
	os.Remove(containerIDPath)
	os.Remove(sessionIDPath)

	// Restart using the agent type command with headless mode, then attach
	// We use headless mode for resilience - if terminal dies again, container survives
	startArgs := []string{"--headless", absDir}
	if err := sessionTypeCommand(agentType, startArgs); err != nil {
		return fmt.Errorf("failed to restart session: %w", err)
	}

	// Now attach to the new container
	fmt.Fprintf(os.Stderr, "\nSession restarted. Attaching...\n")
	return attachCommand([]string{absDir})
}

// attachToContainer attaches to the tmux session in a running container
func attachToContainer(containerName string) error {
	runtime := getContainerRuntime()
	cmd := exec.Command(runtime, "exec", "-it", containerName, "tmux", "attach-session", "-t", "agent-session")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Fprintf(os.Stderr, "Attaching to tmux session in %s...\n", containerName)
	fmt.Fprintf(os.Stderr, "Detach with: Ctrl-b d\n\n")

	return cmd.Run()
}

func logsCommand(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		projectDir    string
		follow        bool
		tail          int
		sessionLogDir string
	)

	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")
	fs.BoolVar(&follow, "follow", false, "Follow log output")
	fs.BoolVar(&follow, "f", false, "Follow log output (shorthand)")
	fs.IntVar(&tail, "tail", 100, "Number of lines to show from end of log")
	fs.IntVar(&tail, "n", 100, "Number of lines to show (shorthand)")
	fs.StringVar(&sessionLogDir, "session-log-dir", "", "Directory for session output logs (default: latest session in project)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	// Find output.log: explicit session-log-dir, or latest session in project
	var logPath string
	if sessionLogDir != "" {
		logPath = filepath.Join(sessionLogDir, "output.log")
	} else {
		// Find the most recent session directory with an output.log
		sessionsDir := getProjectSessionsDir(absDir)
		entries, _ := os.ReadDir(sessionsDir)
		var latestTime time.Time
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			candidate := filepath.Join(sessionsDir, e.Name(), "output.log")
			if info, err := os.Stat(candidate); err == nil && info.ModTime().After(latestTime) {
				latestTime = info.ModTime()
				logPath = candidate
			}
		}
	}

	if logPath == "" || !fileExists(logPath) {
		return fmt.Errorf("no session logs found in %s", absDir)
	}

	// Build tail command
	tailArgs := []string{}
	if follow {
		tailArgs = append(tailArgs, "-f")
	}
	tailArgs = append(tailArgs, "-n", fmt.Sprintf("%d", tail), logPath)

	cmd := exec.Command("tail", tailArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if follow {
		// For follow mode, run interactively
		return cmd.Run()
	}

	// For non-follow mode, capture and output
	output, err := cmd.Output()
	if err != nil {
		return err
	}

	fmt.Print(string(output))

	return writeJSON(resultRecord{
		Timestamp: time.Now().UTC(),
		Type:      "logs",
		Action:    "tail",
		Project:   filepath.Base(absDir),
		Status:    "ok",
		Message:   "logs retrieved",
		Details: map[string]interface{}{
			"log_path": logPath,
			"lines":    tail,
			"follow":   follow,
		},
	})
}

func reloadCommand(args []string) error {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		projectDir  string
		projectName string
	)

	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")
	fs.StringVar(&projectName, "name", "", "Project name (defaults to directory name)")
	fs.StringVar(&projectName, "n", "", "Project name (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	if projectName == "" {
		projectName = filepath.Base(absDir)
	}

	policyPath := filepath.Join(absDir, ".tcv.json")
	if !fileExists(policyPath) {
		return fmt.Errorf("policy file not found: %s\nRun 'tcv init' to create one", policyPath)
	}

	// Check if proxy is running
	ctx := context.Background()
	if _, err := checkProxyStatus(ctx); err != nil {
		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "reload",
			Action:    "reload",
			Project:   projectName,
			Status:    "error",
			Message:   "proxy not running - start it with 'tcv proxy start'",
		})
	}

	// Copy updated policy to proxy volume
	if err := addPolicyToProxy(policyPath, projectName); err != nil {
		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "reload",
			Action:    "reload",
			Project:   projectName,
			Status:    "error",
			Message:   fmt.Sprintf("failed to update policy: %v", err),
		})
	}

	return writeJSON(resultRecord{
		Timestamp:  time.Now().UTC(),
		Type:       "reload",
		Action:     "reload",
		Project:    projectName,
		PolicyFile: policyPath,
		Status:     "ok",
		Message:    "policy reloaded",
		Details: map[string]interface{}{
			"policy_file": policyPath,
		},
	})
}

func baselineCommand(args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		reload   bool
		showHelp bool
	)

	fs.BoolVar(&reload, "reload", false, "Push baseline from repo to running proxy")
	fs.BoolVar(&reload, "r", false, "Push baseline from repo to running proxy (shorthand)")
	fs.BoolVar(&showHelp, "help", false, "Show help")
	fs.BoolVar(&showHelp, "h", false, "Show help (shorthand)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if showHelp {
		fmt.Println(`Usage: tcv baseline [options]

View or update the proxy's baseline policy (always-allowed hosts).

Options:
  -r, --reload    Push baseline-policy.json from repo to running proxy
  -h, --help      Show this help

Examples:
  tcv baseline           # Show current baseline policy
  tcv baseline --reload  # Push repo's baseline-policy.json to proxy`)
		return nil
	}

	// Check if proxy is running
	ctx := context.Background()
	if _, err := checkProxyStatus(ctx); err != nil {
		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "baseline",
			Action:    "baseline",
			Status:    "error",
			Message:   "proxy not running - start it with 'tcv proxy start'",
		})
	}

	if reload {
		// Find baseline file in repo
		tcvRoot := os.Getenv("TCV_ROOT")
		if tcvRoot == "" {
			tcvRoot = "/usr/local/share/tcv"
		}
		baselineFile := filepath.Join(tcvRoot, "images", "tcv-egress", "baseline-policy.json")

		content, err := os.ReadFile(baselineFile)
		if err != nil {
			return writeJSON(resultRecord{
				Timestamp: time.Now().UTC(),
				Type:      "baseline",
				Action:    "reload",
				Status:    "error",
				Message:   fmt.Sprintf("failed to read baseline file %s: %v", baselineFile, err),
			})
		}

		// Post to proxy's update-baseline endpoint
		cmd := exec.CommandContext(ctx, getContainerRuntime(), "exec", proxyContainerName,
			"wget", "-q", "-O-", "--post-data="+string(content), "http://localhost:8081/update-baseline")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return writeJSON(resultRecord{
				Timestamp: time.Now().UTC(),
				Type:      "baseline",
				Action:    "reload",
				Status:    "error",
				Message:   fmt.Sprintf("failed to update baseline: %v - %s", err, string(output)),
			})
		}

		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "baseline",
			Action:    "reload",
			Status:    "ok",
			Message:   "baseline policy updated from repo",
			Details: map[string]interface{}{
				"source": baselineFile,
			},
		})
	}

	// Get current baseline
	cmd := exec.CommandContext(ctx, getContainerRuntime(), "exec", proxyContainerName,
		"wget", "-q", "-O-", "http://localhost:8081/baseline")
	output, err := cmd.Output()
	if err != nil {
		return writeJSON(resultRecord{
			Timestamp: time.Now().UTC(),
			Type:      "baseline",
			Action:    "get",
			Status:    "error",
			Message:   "failed to get baseline policy",
		})
	}

	// Pretty print the baseline
	var baseline map[string]interface{}
	if err := json.Unmarshal(output, &baseline); err != nil {
		fmt.Println(string(output))
	} else {
		pretty, _ := json.MarshalIndent(baseline, "", "  ")
		fmt.Println(string(pretty))
	}

	return nil
}

func initCommand(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		projectDir   string
		projectName  string
		imageType    string
		force        bool
		noPrompt     bool
		localDomains string
		localPorts   string
	)

	fs.StringVar(&projectDir, "project-dir", ".", "Path to project directory")
	fs.StringVar(&projectDir, "d", ".", "Path to project directory (shorthand)")
	fs.StringVar(&projectName, "name", "", "Project name (defaults to directory name)")
	fs.StringVar(&projectName, "n", "", "Project name (shorthand)")
	fs.StringVar(&imageType, "image", "", "Override image type")
	fs.StringVar(&imageType, "i", "", "Override image type (shorthand)")
	fs.BoolVar(&force, "force", false, "Overwrite existing policy file")
	fs.BoolVar(&force, "f", false, "Overwrite existing policy file (shorthand)")
	fs.BoolVar(&noPrompt, "no-prompt", false, "Skip interactive image selection, use detected image")
	fs.BoolVar(&noPrompt, "y", false, "Skip interactive image selection (shorthand)")
	fs.StringVar(&localDomains, "local-domains", "", "Comma-separated local domains (e.g., myapp.local,api.myapp.local)")
	fs.StringVar(&localPorts, "local-ports", "", "Comma-separated local ports (e.g., 8000,5173)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use remaining arg as project dir if provided
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return err
	}

	// Default project name to directory name
	if projectName == "" {
		projectName = filepath.Base(absDir)
	}

	policyPath := filepath.Join(absDir, ".tcv.json")

	if fileExists(policyPath) && !force {
		return fmt.Errorf("policy file already exists: %s\nUse --force to overwrite", policyPath)
	}

	// Detect project type
	projectType, detectedImage := detectProjectType(absDir)

	// If no --image flag provided, prompt for selection in interactive mode
	if imageType == "" {
		if !noPrompt && term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Printf("Detected project type: %s\n", projectType)
			selected, err := promptImageSelection(detectedImage)
			if err != nil {
				return fmt.Errorf("image selection failed: %w", err)
			}
			imageType = selected
		} else {
			// Non-interactive or --no-prompt: use detected image
			imageType = detectedImage
		}
	}

	// Parse local domains and ports
	var domains []string
	var ports []int
	if localDomains != "" {
		domains = strings.Split(localDomains, ",")
		for i := range domains {
			domains[i] = strings.TrimSpace(domains[i])
		}
	}
	if localPorts != "" {
		for _, p := range strings.Split(localPorts, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				var port int
				fmt.Sscanf(p, "%d", &port)
				if port > 0 {
					ports = append(ports, port)
				}
			}
		}
	}

	// Generate policy based on project type
	policy := generatePolicy(projectName, projectType, imageType, domains, ports)

	// Write policy file
	policyJSON, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(policyPath, policyJSON, 0644); err != nil {
		return err
	}

	// Add policy and container files to .gitignore if it exists
	gitignorePath := filepath.Join(absDir, ".gitignore")
	if fileExists(gitignorePath) {
		addToGitignore(gitignorePath, ".tcv.json")
		addToGitignore(gitignorePath, ".container_id")
	}

	return writeJSON(resultRecord{
		Timestamp:  time.Now().UTC(),
		Type:       "init",
		Action:     "create",
		Project:    filepath.Base(absDir),
		PolicyFile: policyPath,
		Status:     "ok",
		Message:    fmt.Sprintf("created policy for %s project", projectType),
		Details: map[string]interface{}{
			"project_type":        projectType,
			"image_type":          imageType,
			"local_domains":       domains,
			"local_ports":         ports,
		},
	})
}

func detectProjectType(dir string) (string, string) {
	// Check for various project indicators
	if fileExists(filepath.Join(dir, "pubspec.yaml")) {
		return "flutter", "agent-flutter-vcs"
	}
	if fileExists(filepath.Join(dir, "angular.json")) {
		return "angular", "agent-angular-vcs"
	}
	if fileExists(filepath.Join(dir, "composer.json")) {
		composerData, _ := os.ReadFile(filepath.Join(dir, "composer.json"))
		if strings.Contains(string(composerData), `"laravel/framework"`) {
			// Check for hybrid Laravel + Angular
			if fileExists(filepath.Join(dir, "package.json")) {
				pkgData, _ := os.ReadFile(filepath.Join(dir, "package.json"))
				if strings.Contains(string(pkgData), `"angular"`) || strings.Contains(string(pkgData), `"@angular"`) {
					return "laravel-angular", "agent-laravel-angular-vcs"
				}
			}
			return "laravel", "agent-laravel-vcs"
		}
		return "php", "agent-php-vcs"
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		// Check for Go + Nuxt hybrid
		if fileExists(filepath.Join(dir, "package.json")) {
			pkgData, _ := os.ReadFile(filepath.Join(dir, "package.json"))
			if strings.Contains(string(pkgData), `"nuxt"`) {
				return "go-nuxt", "agent-go-nuxt-vcs"
			}
		}
		return "go", "tcv-agent-base"
	}
	if fileExists(filepath.Join(dir, "package.json")) {
		pkgData, _ := os.ReadFile(filepath.Join(dir, "package.json"))
		if strings.Contains(string(pkgData), `"nuxt"`) {
			return "nuxt", "agent-node-vcs"
		}
		return "node", "agent-node-vcs"
	}
	if fileExists(filepath.Join(dir, "requirements.txt")) || fileExists(filepath.Join(dir, "pyproject.toml")) {
		return "python", "agent-python-vcs"
	}
	if fileExists(filepath.Join(dir, "main.tf")) || fileExists(filepath.Join(dir, "terraform.tf")) {
		return "tofu", "agent-tofu-vcs"
	}

	return "generic", "tcv-agent-base"
}

func generatePolicy(projectName, projectType, imageType string, localDomains []string, localPorts []int) map[string]interface{} {
	baseHosts := []string{
		"github.com:443",
		"api.github.com:443",
		"codeload.github.com:443",
		"raw.githubusercontent.com:443",
		"api.anthropic.com:443",
		"statsig.anthropic.com:443",
		"console.anthropic.com:443",
		"api.openai.com:443",
	}

	policy := map[string]interface{}{
		"project_name": projectName,
		"sessions": map[string]interface{}{
			"claude": map[string]interface{}{
				"image":   imageType,
				"command": "claude",
				"args":    []string{"--dangerously-skip-permissions"},
				"default": true,
			},
			"codex": map[string]interface{}{
				"image":   imageType,
				"command": "codex",
				"args":    []string{"--full-auto"},
			},
		},
		"env_allow": []string{
			"ANTHROPIC_API_KEY",
			"OPENAI_API_KEY",
		},
		"network": map[string]interface{}{
			"enabled":     true,
			"proxy":       "http://tcv-egress:8080",
			"allow_ips":   []string{},
			"allow_hosts": baseHosts,
		},
	}

	// Add project-specific settings
	switch projectType {
	case "laravel", "php":
		policy["allowed_tools"] = []string{"php", "composer", "git", "jq", "bash", "artisan", "phpunit", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"APP_ENV", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "COMPOSER_MEMORY_LIMIT", "XDEBUG_MODE", "PHP_IDE_CONFIG"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"repo.packagist.org:443",
			"packagist.org:443",
			"getcomposer.org:443",
		}, baseHosts...)

	case "laravel-angular":
		policy["allowed_tools"] = []string{"php", "composer", "node", "npm", "npx", "git", "jq", "bash", "artisan", "phpunit", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"npm":  {"install", "run", "test", "build", "start"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"APP_ENV", "NODE_ENV", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "COMPOSER_MEMORY_LIMIT", "XDEBUG_MODE", "PHP_IDE_CONFIG"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"repo.packagist.org:443",
			"packagist.org:443",
			"getcomposer.org:443",
			"registry.npmjs.org:443",
		}, baseHosts...)

	case "node", "nuxt":
		policy["allowed_tools"] = []string{"node", "npm", "npx", "git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"npm":  {"install", "run", "test", "build", "start"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"NODE_ENV", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"registry.npmjs.org:443",
		}, baseHosts...)

	case "go", "go-nuxt":
		tools := []string{"go", "git", "jq", "bash", "curl"}
		subcommands := map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"go":   {"build", "run", "test", "mod", "get", "fmt", "vet", "generate"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		envAllow := []string{"GOPATH", "GOPROXY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		hosts := []string{
			"proxy.golang.org:443",
			"sum.golang.org:443",
			"storage.googleapis.com:443",
		}

		if projectType == "go-nuxt" {
			tools = append(tools, "node", "npm", "npx", "nuxi")
			subcommands["npm"] = []string{"install", "run", "test", "build", "start"}
			subcommands["nuxi"] = []string{"dev", "build", "generate", "preview", "prepare"}
			envAllow = append(envAllow, "NODE_ENV", "NUXT_ENV")
			hosts = append(hosts, "registry.npmjs.org:443")
		}

		policy["allowed_tools"] = tools
		policy["allowed_subcommands"] = subcommands
		policy["env_allow"] = envAllow
		policy["network"].(map[string]interface{})["allow_hosts"] = append(hosts, baseHosts...)

	case "python":
		policy["allowed_tools"] = []string{"python", "python3", "pip", "pip3", "git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"pip":  {"install", "list", "show"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"PYTHONPATH", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"pypi.org:443",
			"files.pythonhosted.org:443",
		}, baseHosts...)

	case "flutter":
		policy["allowed_tools"] = []string{"flutter", "dart", "git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":     {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"flutter": {"run", "build", "test", "pub", "doctor", "clean"},
			"dart":    {"run", "pub", "test", "analyze"},
			"curl":    {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"FLUTTER_ROOT", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"pub.dev:443",
			"storage.googleapis.com:443",
		}, baseHosts...)

	case "angular":
		policy["allowed_tools"] = []string{"node", "npm", "npx", "ng", "git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"npm":  {"install", "run", "test", "build", "start"},
			"ng":   {"serve", "build", "test", "generate", "update"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"NODE_ENV", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"registry.npmjs.org:443",
		}, baseHosts...)

	case "tofu":
		policy["allowed_tools"] = []string{"tofu", "terraform", "git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":       {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"tofu":      {"init", "plan", "apply", "destroy", "validate", "fmt", "output", "show", "state", "import", "refresh", "providers"},
			"terraform": {"init", "plan", "apply", "destroy", "validate", "fmt", "output", "show", "state", "import", "refresh", "providers"},
			"curl":      {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
		policy["env_allow"] = []string{"TF_VAR_*", "TF_LOG", "TF_DATA_DIR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}
		policy["network"].(map[string]interface{})["allow_hosts"] = append([]string{
			"registry.opentofu.org:443",
			"releases.hashicorp.com:443",
			"checkpoint-api.hashicorp.com:443",
			"registry.terraform.io:443",
		}, baseHosts...)

	default:
		policy["allowed_tools"] = []string{"git", "jq", "bash", "curl"}
		policy["allowed_subcommands"] = map[string][]string{
			"git":  {"status", "add", "commit", "switch", "checkout", "merge", "rebase", "log", "diff", "restore", "reset", "rev-parse", "config"},
			"curl": {"-I", "--head", "-L", "-s", "-S", "-f", "--proto", "--tlsv1.2"},
		}
	}

	// Add local domains and ports if specified
	if len(localDomains) > 0 {
		policy["local_domains"] = localDomains
	}
	if len(localPorts) > 0 {
		policy["local_ports"] = localPorts
	}

	return policy
}

func addToGitignore(path, entry string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	if strings.Contains(string(data), entry) {
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	// Add newline if file doesn't end with one
	if len(data) > 0 && data[len(data)-1] != '\n' {
		f.WriteString("\n")
	}
	f.WriteString(entry + "\n")
}

func buildCommand(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		all      bool
		noCache  bool
		listOnly bool
	)

	fs.BoolVar(&all, "all", false, "Build all images")
	fs.BoolVar(&noCache, "no-cache", false, "Build without cache")
	fs.BoolVar(&listOnly, "list", false, "List available images without building")

	if err := fs.Parse(args); err != nil {
		return err
	}

	// Use shared function to list available images
	images, err := listAvailableImages()
	if err != nil {
		return err
	}

	if listOnly {
		fmt.Println("Available images:")
		fmt.Println()
		fmt.Println("Core images (images/):")
		for _, img := range images {
			if img.Type == "core" {
				fmt.Printf("  - %s\n", img.Name)
			}
		}
		fmt.Println()
		fmt.Println("Custom images (images-custom/):")
		for _, img := range images {
			if img.Type == "custom" {
				fmt.Printf("  - %s\n", img.Name)
			}
		}
		return nil
	}

	// Determine which images to build
	var toBuild []imageInfo

	if all {
		toBuild = images
	} else if fs.NArg() > 0 {
		// Build specific images
		requested := make(map[string]bool)
		for _, arg := range fs.Args() {
			requested[arg] = true
		}
		for _, img := range images {
			if requested[img.Name] {
				toBuild = append(toBuild, img)
				delete(requested, img.Name)
			}
		}
		for name := range requested {
			fmt.Fprintf(os.Stderr, "warning: image not found: %s\n", name)
		}
	} else {
		fmt.Println("Usage: tcv build [--all] [--no-cache] [--list] [image-name...]")
		fmt.Println()
		fmt.Println("Use --list to see available images")
		return nil
	}

	if len(toBuild) == 0 {
		return fmt.Errorf("no images to build")
	}

	// Sort images so base images are built first (custom images depend on them)
	// Priority: tcv-agent-base first, then other core images, then custom images
	sort.Slice(toBuild, func(i, j int) bool {
		// tcv-agent-base always first (custom images depend on it)
		if toBuild[i].Name == "tcv-agent-base" {
			return true
		}
		if toBuild[j].Name == "tcv-agent-base" {
			return false
		}
		// Core images before custom images
		if toBuild[i].Type != toBuild[j].Type {
			return toBuild[i].Type == "core"
		}
		// Alphabetical within same type
		return toBuild[i].Name < toBuild[j].Name
	})

	// Build images
	var built []string
	var failed []string

	// Generate cache bust value (timestamp) to ensure agent CLIs are always updated
	cacheBust := fmt.Sprintf("%d", time.Now().Unix())

	for _, img := range toBuild {
		fmt.Printf("Building %s (%s)...\n", img.Name, img.Type)

		buildArgs := []string{"build"}
		if noCache {
			buildArgs = append(buildArgs, "--no-cache")
		}
		// Always pass cache bust arg to force fresh agent installs
		buildArgs = append(buildArgs, "--build-arg", "AGENT_CACHE_BUST="+cacheBust)
		buildArgs = append(buildArgs,
			"-t", fmt.Sprintf("localhost/%s:latest", img.Name),
			"-t", fmt.Sprintf("localhost/%s:1.0", img.Name),
			"-f", filepath.Join(img.Path, "Containerfile"),
			img.Context, // use context (may differ from Path for images needing repo root)
		)

		cmd := exec.Command(getContainerRuntime(), buildArgs...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to build %s: %v\n", img.Name, err)
			failed = append(failed, img.Name)
		} else {
			fmt.Printf("✓ Built %s\n", img.Name)
			built = append(built, img.Name)
		}
	}

	fmt.Println()
	if len(built) > 0 {
		fmt.Printf("Successfully built: %s\n", strings.Join(built, ", "))
	}
	if len(failed) > 0 {
		fmt.Printf("Failed: %s\n", strings.Join(failed, ", "))
		return fmt.Errorf("%d image(s) failed to build", len(failed))
	}

	return nil
}

func proxyEnsureCommand() error {
	res, err := ensureProxy(context.Background())
	if err != nil {
		return err
	}

	return writeJSON(resultRecord{
		Timestamp: time.Now().UTC(),
		Type:      "proxy",
		Action:    "start",
		Status:    res.Status,
		Message:   "proxy started",
		Details: map[string]interface{}{
			"output": res.Output,
		},
	})
}

func proxyStopCommand() error {
	ctx := context.Background()

	// Stop and remove the proxy container
	stopCmd := exec.CommandContext(ctx, getContainerRuntime(), "stop", proxyContainerName)
	stopCmd.Run() // Ignore error if not running

	rmCmd := exec.CommandContext(ctx, getContainerRuntime(), "rm", proxyContainerName)
	rmCmd.Run() // Ignore error if not exists

	return writeJSON(resultRecord{
		Timestamp: time.Now().UTC(),
		Type:      "proxy",
		Action:    "stop",
		Status:    "stopped",
		Message:   "proxy stopped",
	})
}

func checkProxyStatus(ctx context.Context) (proxyResult, error) {
	cmd := exec.CommandContext(ctx, getContainerRuntime(), "ps", "--filter", "name="+proxyContainerName, "--filter", "status=running", "--format", "{{.Names}}")
	output, err := cmd.Output()
	if err != nil {
		return proxyResult{Status: "stopped", Output: ""}, err
	}
	if strings.TrimSpace(string(output)) == proxyContainerName {
		return proxyResult{Status: "running", Output: "proxy is running"}, nil
	}
	return proxyResult{Status: "stopped", Output: ""}, fmt.Errorf("proxy not running")
}

func ensureProxy(ctx context.Context) (proxyResult, error) {
	// Check if proxy is already running
	if res, err := checkProxyStatus(ctx); err == nil {
		return res, nil
	}

	// Remove stale container if exists
	exec.CommandContext(ctx, getContainerRuntime(), "rm", "-f", proxyContainerName).Run()

	// Ensure network exists
	if err := ensureNetwork(ctx); err != nil {
		return proxyResult{Status: "error"}, fmt.Errorf("failed to create network: %w", err)
	}

	// Ensure policy volume exists
	if err := ensurePolicyVolume(ctx); err != nil {
		return proxyResult{Status: "error"}, fmt.Errorf("failed to create policy volume: %w", err)
	}

	// Start proxy container with host networking
	proxyImage := resolveImageName(proxyImageName)
	proxyPort := getProxyPort()
	args := []string{
		"run", "-d",
		"--name", proxyContainerName,
		"--network", "host",
		"-e", "POLICY_DIR=/policy",
		"-e", "LOG_FILE=/dev/stdout",
		"-e", "PORT=" + proxyPort,
		"-v", policyVolumeName + ":/policy:ro",
		"--restart", "unless-stopped",
		proxyImage,
	}

	cmd := exec.CommandContext(ctx, getContainerRuntime(), args...)
	cmd.Env = getContainerCmdEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return proxyResult{
			Status: "error",
			Output: string(output),
		}, fmt.Errorf("failed to start proxy: %w", err)
	}

	return proxyResult{Status: "started", Output: string(output)}, nil
}

func ensureNetwork(ctx context.Context) error {
	// Check if network exists
	checkCmd := exec.CommandContext(ctx, getContainerRuntime(), "network", "exists", networkName)
	checkCmd.Env = getContainerCmdEnv()
	if checkCmd.Run() == nil {
		return nil
	}

	// Create network
	createCmd := exec.CommandContext(ctx, getContainerRuntime(), "network", "create", networkName)
	createCmd.Env = getContainerCmdEnv()
	return createCmd.Run()
}

func ensurePolicyVolume(ctx context.Context) error {
	// Check if volume exists
	checkCmd := exec.CommandContext(ctx, getContainerRuntime(), "volume", "exists", policyVolumeName)
	checkCmd.Env = getContainerCmdEnv()
	if checkCmd.Run() == nil {
		return nil
	}

	// Create volume
	createCmd := exec.CommandContext(ctx, getContainerRuntime(), "volume", "create", policyVolumeName)
	createCmd.Env = getContainerCmdEnv()
	return createCmd.Run()
}

func sanitizePolicyName(projectName string) string {
	// Use only the base name and strip anything that isn't alphanumeric, dash, or underscore
	base := filepath.Base(projectName)
	var safe strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			safe.WriteRune(r)
		}
	}
	if safe.Len() == 0 {
		return "default"
	}
	return safe.String()
}

func addPolicyToProxy(policyPath, projectName string) error {
	ctx := context.Background()
	policyName := sanitizePolicyName(projectName) + ".json"

	// Copy policy file to proxy volume
	cmd := exec.CommandContext(ctx, getContainerRuntime(), "cp",
		policyPath, proxyContainerName+":/policy/"+policyName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy policy: %w", err)
	}

	// Reload proxy if running
	reloadProxy(ctx)
	return nil
}

func removePolicyFromProxy(projectName string) {
	ctx := context.Background()
	policyName := sanitizePolicyName(projectName) + ".json"

	// Remove policy file from proxy container
	cmd := exec.CommandContext(ctx, getContainerRuntime(), "exec",
		proxyContainerName, "rm", "-f", "/policy/"+policyName)
	cmd.Run()

	// Reload proxy if running
	reloadProxy(ctx)
}

func reloadProxy(ctx context.Context) {
	// Check if proxy is running
	if _, err := checkProxyStatus(ctx); err != nil {
		return
	}

	// Send reload signal via HTTP
	cmd := exec.CommandContext(ctx, getContainerRuntime(), "exec", proxyContainerName,
		"wget", "-q", "-O-", "--post-data=", "http://localhost:8081/reload")
	cmd.Run()
}

func isContainerRunning(name string) bool {
	cmd := exec.Command(getContainerRuntime(), "ps", "--filter", "name="+name, "--format", "{{.Names}}")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == name
}

func getGitConfig(key, defaultVal string) string {
	cmd := exec.Command("git", "config", "--global", key)
	output, err := cmd.Output()
	if err != nil {
		return defaultVal
	}
	val := strings.TrimSpace(string(output))
	if val == "" {
		return defaultVal
	}
	return val
}

func getGitBranch(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func getGitCommit(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func isGitDirty(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(output))) > 0
}

func readPolicy(path string) (*policyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg policyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.ImageType == "" {
		cfg.ImageType = "agent-php-vcs"
	}
	// Migrate legacy agents+image_type to sessions format
	migratePolicyToSessions(&cfg)
	return &cfg, nil
}

// migratePolicyToSessions converts legacy agents config to sessions format in memory.
// If Sessions is already defined, no migration is performed.
func migratePolicyToSessions(policy *policyConfig) {
	// If sessions already defined, nothing to migrate
	if len(policy.Sessions) > 0 {
		return
	}

	// If no agents defined, create default sessions based on image type
	if len(policy.Agents) == 0 {
		policy.Sessions = map[string]sessionConfig{
			"claude": {
				Image:   policy.ImageType,
				Command: "claude",
				Args:    []string{"--dangerously-skip-permissions"},
				Default: true,
			},
		}
		return
	}

	// Migrate agents to sessions, using ImageType for all
	policy.Sessions = make(map[string]sessionConfig, len(policy.Agents))
	for name, agent := range policy.Agents {
		policy.Sessions[name] = sessionConfig{
			Image:   policy.ImageType,
			Command: agent.Command,
			Args:    agent.Args,
			Default: agent.Default,
		}
	}
}

// resolveSessionConfig finds a session config by name or returns the default.
// Returns the session config, its name, and any error.
func resolveSessionConfig(policy *policyConfig, sessionName string) (*sessionConfig, string, error) {
	if len(policy.Sessions) == 0 {
		return nil, "", fmt.Errorf("no sessions configured in policy")
	}

	// If session name specified, look it up
	if sessionName != "" {
		session, ok := policy.Sessions[sessionName]
		if !ok {
			available := make([]string, 0, len(policy.Sessions))
			for name := range policy.Sessions {
				available = append(available, name)
			}
			sort.Strings(available)
			return nil, "", fmt.Errorf("session '%s' not found in policy. Available: %s",
				sessionName, strings.Join(available, ", "))
		}
		return &session, sessionName, nil
	}

	// Find default session
	for name, session := range policy.Sessions {
		if session.Default {
			s := session // avoid loop variable capture
			return &s, name, nil
		}
	}

	// No default set, use first alphabetically
	var names []string
	for name := range policy.Sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	session := policy.Sessions[names[0]]
	return &session, names[0], nil
}

// checkExistingSession verifies no session is already running for this project.
// Returns an error if a session is running, nil otherwise.
func checkExistingSession(projectDir, projectName string) error {
	containerIDPath := filepath.Join(projectDir, ".container_id")
	if !fileExists(containerIDPath) {
		return nil
	}

	containerNameBytes, err := os.ReadFile(containerIDPath)
	if err != nil {
		return nil // File exists but unreadable, proceed
	}

	containerName := strings.TrimSpace(string(containerNameBytes))
	if containerName == "" {
		return nil
	}

	// Check if container is actually running
	containers, _ := listProjectContainers(projectName, containerName)
	for _, c := range containers {
		if strings.HasPrefix(strings.ToLower(c.Status), "up") ||
			strings.Contains(strings.ToLower(c.Status), "running") {
			return fmt.Errorf("session already running for this project (container: %s)\nStop it first with: tcv stop", containerName)
		}
	}

	// Container not running, clean up stale files
	os.Remove(containerIDPath)
	sessionIDPath := filepath.Join(projectDir, ".session_id")
	os.Remove(sessionIDPath)

	return nil
}

// resolveAgentCommand determines the command to run based on policy and flags
// Priority: cmdOverride > agentName from policy > default agent from policy > fallback
func resolveAgentCommand(policy *policyConfig, agentName, cmdOverride string) (string, error) {
	// If explicit command override, use it
	if cmdOverride != "" {
		return cmdOverride, nil
	}

	// If no agents configured, fall back to default
	if len(policy.Agents) == 0 {
		return "claude --dangerously-skip-permissions", nil
	}

	// If agent name specified, look it up
	if agentName != "" {
		agent, ok := policy.Agents[agentName]
		if !ok {
			available := make([]string, 0, len(policy.Agents))
			for name := range policy.Agents {
				available = append(available, name)
			}
			return "", fmt.Errorf("agent '%s' not found in policy. Available: %s", agentName, strings.Join(available, ", "))
		}
		return buildAgentCommand(agent), nil
	}

	// Find default agent
	for _, agent := range policy.Agents {
		if agent.Default {
			return buildAgentCommand(agent), nil
		}
	}

	// No default set, use first agent (alphabetically for consistency)
	var names []string
	for name := range policy.Agents {
		names = append(names, name)
	}
	if len(names) > 0 {
		sort.Strings(names)
		return buildAgentCommand(policy.Agents[names[0]]), nil
	}

	return "bash", nil
}

func buildAgentCommand(agent agentConfig) string {
	if len(agent.Args) == 0 {
		return agent.Command
	}
	return agent.Command + " " + strings.Join(agent.Args, " ")
}

func loadEnvFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var env []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "=") {
			return nil, fmt.Errorf("invalid env line: %s", line)
		}
		env = append(env, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return env, nil
}

func listProjectContainers(project string, exact string) ([]containerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := []string{"ps", "-a", "--format", "{{.Names}}||{{.Status}}||{{.ID}}"}
	if exact != "" {
		args = append(args, "--filter", fmt.Sprintf("name=%s", exact))
	} else {
		args = append(args, "--filter", fmt.Sprintf("name=agent-%s-", project))
	}

	out, err := runAndCapture(ctx, getContainerRuntime(), args, "", os.Environ())
	if err != nil {
		if exitCode(err) == 125 {
			return nil, fmt.Errorf("container runtime (%s) is required to query containers: %w", getContainerRuntime(), err)
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var containers []containerInfo
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "||")
		info := containerInfo{Name: parts[0]}
		if len(parts) > 1 {
			info.Status = parts[1]
		}
		if len(parts) > 2 {
			info.ID = parts[2]
		}
		containers = append(containers, info)
	}
	return containers, nil
}

func runAndCapture(ctx context.Context, command string, args []string, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = env
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

func isDirectory(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// cleanupOldSessions removes session directories older than 7 days.
// This runs on session start to prevent accumulation of stale session files.
func cleanupOldSessions(projectDir string) {
	sessionsDir := filepath.Join(projectDir, ".tcv", "sessions")
	if !isDirectory(sessionsDir) {
		return
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		sessionPath := filepath.Join(sessionsDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(sessionPath); err == nil {
				fmt.Fprintf(os.Stderr, "Cleaned up old session: %s\n", entry.Name())
			}
		}
	}
}

// getProxyPort returns the configured proxy port (default 8080)
func getProxyPort() string {
	if port := os.Getenv("TCV_PROXY_PORT"); port != "" {
		return port
	}
	return defaultProxyPort
}

// Conventional install location (symlink created by install.sh)
const installSharePath = "/usr/local/share/tcv"

// getRepoRoot returns the path to the monorepo root directory
func getRepoRoot() string {
	// 1. Environment variable override
	if v := os.Getenv("TCV_ROOT"); v != "" {
		return v
	}

	// 2. Conventional install location (symlink to repo)
	if fileExists(filepath.Join(installSharePath, "images")) {
		return installSharePath
	}

	// 3. Development mode: relative to executable (cli/tcv -> repo root)
	exe, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "..")
		if fileExists(filepath.Join(candidate, "images")) {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}

	return ""
}

// getInfraDir is deprecated, use getRepoRoot instead
// Kept for backwards compatibility
func getInfraDir() string {
	return getRepoRoot()
}

// getImagesDir returns the path to core images directory
func getImagesDir() string {
	infraDir := getInfraDir()
	if infraDir != "" {
		return filepath.Join(infraDir, "images")
	}
	return ""
}

// getCustomImagesDir returns the path to custom images directory
func getCustomImagesDir() string {
	infraDir := getInfraDir()
	if infraDir != "" {
		return filepath.Join(infraDir, "images-custom")
	}
	return ""
}

// resolveImageName converts an image type to a full image reference
// Looks in images/ first, then images-custom/, then falls back to localhost/
func resolveImageName(imageType string) string {
	// Strip version suffix if present
	baseName := imageType
	version := "latest"
	if idx := strings.LastIndex(imageType, ":"); idx != -1 {
		baseName = imageType[:idx]
		version = imageType[idx+1:]
	}

	// Check if image exists in core images
	imagesDir := getImagesDir()
	if imagesDir != "" && fileExists(filepath.Join(imagesDir, baseName, "Containerfile")) {
		return fmt.Sprintf("localhost/%s:%s", baseName, version)
	}

	// Check if image exists in custom images
	customDir := getCustomImagesDir()
	if customDir != "" && fileExists(filepath.Join(customDir, baseName, "Containerfile")) {
		return fmt.Sprintf("localhost/%s:%s", baseName, version)
	}

	// Return as-is with localhost prefix
	if !strings.Contains(baseName, "/") {
		return fmt.Sprintf("localhost/%s:%s", baseName, version)
	}

	return imageType
}

func writeJSON(record resultRecord) error {
	record.Timestamp = record.Timestamp.UTC()
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(record)
}

func printUsage() {
	fmt.Printf(`tcv - Agent Orchestrator CLI v%s

Usage: tcv <command> [options] [project-dir]

Session Commands:
  claude [dir]         Start Claude session (recommended)
  codex [dir]          Start Codex session
  gemini [dir]         Start Gemini session

Management Commands:
  stop [dir]           Stop containers for the project (graceful)
  kill [dir]           Kill containers for the project (forceful)
  status [dir]         Inspect current container state
  logs [dir]           Tail container output logs
  attach [dir]         Attach to tmux session in running container
  reconnect [dir]      Reconnect to crashed session (restarts if needed)

Setup Commands:
  init [dir]           Initialize project with .tcv.json
  reload [dir]         Reload .tcv.json policy into running proxy
  baseline             View or update proxy baseline policy
  proxy <cmd>          Manage egress proxy (start, stop, status)
  build [images]       Build container images from images/ and images-custom/

Legacy Commands (deprecated):
  start [session] [dir]  Start session (use claude/codex/gemini instead)

Session Options:
  -d, --project-dir      Path to project directory (default: current dir)
  -n, --project-name     Logical project name (default: directory name)
      --headless         Run in headless mode with tmux+script PTY wrapper
      --session-log-dir  Directory for session output logs
      --timeout          Timeout for session (e.g. 10m)
      --env-file         Path to KEY=VALUE env file
      --policy           Override policy file path

Logs Options:
  -f, --follow           Follow log output
  -n, --tail             Number of lines to show (default: 100)

Init Options:
  -f, --force            Overwrite existing policy file
      --image            Override image type
      --local-domains    Comma-separated local domains (e.g., myapp.local)
      --local-ports      Comma-separated local ports (e.g., 8000,5173)

Reload Options:
  -d, --project-dir      Path to project directory (default: current dir)
  -n, --name             Project name (default: directory name)

Build Options:
      --all              Build all images (core + custom)
      --no-cache         Build without using cache
      --list             List available images without building

Environment Variables:
  TCV_ROOT           Override path to tcv repo (default: /usr/local/share/tcv)
  TCV_PROXY_PORT     Egress proxy port (default: 8080)
  ANTHROPIC_API_KEY      API key for Claude
  OPENAI_API_KEY         API key for Codex

Examples:
  tcv init                        # Initialize current directory
  tcv init ~/projects/myapp       # Initialize specific project
  tcv claude                      # Start Claude in current directory
  tcv claude ~/projects/myapp     # Start Claude in myproject
  tcv gemini                      # Start Gemini session
  tcv codex ./myproject           # Start Codex in myproject
  tcv logs -f                     # Follow session logs
  tcv attach                      # Attach to tmux session (for debugging)
  tcv reconnect                   # Reconnect after terminal crash
  tcv stop                        # Stop running session
  tcv reload                      # Reload .tcv.json after editing allowed_hosts
  tcv proxy start                 # Ensure egress proxy is running
  tcv build --list                # List available images
  tcv build tcv-egress        # Build specific image
  tcv build --all                 # Build all images
`, version)
}
