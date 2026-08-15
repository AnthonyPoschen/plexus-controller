package factorio

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	factoriov1 "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	defaultBinary       = "/opt/factorio/bin/x64/factorio"
	defaultDataDir      = "/factorio"
	defaultGamePort     = 34197
	defaultRCONPort     = 27015
	rconRetryInterval   = 200 * time.Millisecond
	settingsUsernameKey = "username"
	settingsTokenKey    = "token"
	settingsPasswordKey = "game_password"
)

// Paths is the on-disk Factorio layout the controller materializes before boot.
type Paths struct {
	Binary       string
	DataDir      string
	SavesDir     string
	ModsDir      string
	SettingsFile string
	ConfigINI    string
	GamePort     int
	RCONPort     int
}

// DefaultPaths is the Factorio PVC and image layout used in the game pod.
func DefaultPaths() Paths {
	return Paths{
		Binary:       defaultBinary,
		DataDir:      defaultDataDir,
		SavesDir:     filepath.Join(defaultDataDir, "saves"),
		ModsDir:      filepath.Join(defaultDataDir, "mods"),
		SettingsFile: filepath.Join(defaultDataDir, "config", factoriov1.ConfigFileName),
		ConfigINI:    filepath.Join(defaultDataDir, "config", "config.ini"),
		GamePort:     defaultGamePort,
		RCONPort:     defaultRCONPort,
	}
}

// Adapter boots Factorio from a hosted save and stops it with the adapter
// RCON /quit sequence. It does not watch Kubernetes.
type Adapter struct {
	Paths     Paths
	LookupEnv func(string) (string, bool)
	Dial      func(ctx context.Context, network, address string) (net.Conn, error)
	Updater   GameUpdater
}

func (a Adapter) Name() string { return factoriov1.GameID }

// UpdateOnBoot replaces the image seed with the latest dedicated-server build
// of the selected channel before the game process starts.
func (a Adapter) UpdateOnBoot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, present := a.env(factoriov1.ChannelEnv)
	channel, err := selectedChannel(value, present)
	if err != nil {
		return err
	}
	return a.updater().Update(ctx, channel)
}

func (a Adapter) GracePeriod() time.Duration {
	timeout := factoriov1.Schema().Shutdown.TimeoutSeconds
	if timeout < 1 {
		timeout = 90
	}
	return time.Duration(timeout) * time.Second
}

func (a Adapter) Command(ctx context.Context) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	paths := a.paths()
	password, ok := a.env("RCON_PASSWORD")
	if !ok || password == "" {
		return nil, fmt.Errorf("RCON_PASSWORD is required to boot Factorio")
	}
	for _, dir := range []string{paths.SavesDir, paths.ModsDir, filepath.Dir(paths.SettingsFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare Factorio directories: %w", err)
		}
	}
	if err := a.mergeSettings(paths.SettingsFile); err != nil {
		return nil, err
	}
	if err := writeConfigINI(paths.ConfigINI, paths.DataDir); err != nil {
		return nil, err
	}
	saveName, err := selectHostedSave(paths.SavesDir)
	if err != nil {
		return nil, err
	}
	savePath := filepath.Join(paths.SavesDir, saveName)
	cmd := exec.Command(paths.Binary,
		"--config", paths.ConfigINI,
		"--mod-directory", paths.ModsDir,
		"--start-server", savePath,
		"--server-settings", paths.SettingsFile,
		"--port", strconv.Itoa(paths.GamePort),
		"--rcon-port", strconv.Itoa(paths.RCONPort),
		"--rcon-password", password,
	)
	cmd.Dir = paths.DataDir
	return cmd, nil
}

func (a Adapter) GracefulStop(ctx context.Context, _ *os.Process) error {
	password, ok := a.env("RCON_PASSWORD")
	if !ok || password == "" {
		return fmt.Errorf("RCON_PASSWORD is required for Factorio graceful stop")
	}
	command := factoriov1.Schema().Shutdown.Command
	if command == "" {
		command = "/quit"
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.paths().RCONPort))
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("factorio graceful stop: %w", last)
			}
			return fmt.Errorf("factorio graceful stop: %w", err)
		}
		conn, err := a.dial(ctx, addr)
		if err != nil {
			last = err
			if sleepErr := sleepContext(ctx, rconRetryInterval); sleepErr != nil {
				return fmt.Errorf("factorio graceful stop: %w", err)
			}
			continue
		}
		err = runRCON(ctx, conn, password, command)
		_ = conn.Close()
		if err == nil {
			return nil
		}
		last = err
		if sleepErr := sleepContext(ctx, rconRetryInterval); sleepErr != nil {
			return fmt.Errorf("factorio graceful stop: %w", err)
		}
	}
}

func (a Adapter) paths() Paths {
	paths := a.Paths
	defaults := DefaultPaths()
	if paths.Binary == "" {
		paths.Binary = defaults.Binary
	}
	if paths.DataDir == "" {
		paths.DataDir = defaults.DataDir
	}
	if paths.SavesDir == "" {
		paths.SavesDir = defaults.SavesDir
	}
	if paths.ModsDir == "" {
		paths.ModsDir = defaults.ModsDir
	}
	if paths.SettingsFile == "" {
		paths.SettingsFile = defaults.SettingsFile
	}
	if paths.ConfigINI == "" {
		paths.ConfigINI = defaults.ConfigINI
	}
	if paths.GamePort == 0 {
		paths.GamePort = defaults.GamePort
	}
	if paths.RCONPort == 0 {
		paths.RCONPort = defaults.RCONPort
	}
	return paths
}

func (a Adapter) updater() GameUpdater {
	if a.Updater != nil {
		return a.Updater
	}
	return SteamcmdUpdater{}
}

func (a Adapter) env(name string) (string, bool) {
	if a.LookupEnv != nil {
		return a.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func (a Adapter) dial(ctx context.Context, addr string) (net.Conn, error) {
	if a.Dial != nil {
		return a.Dial(ctx, "tcp", addr)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

func (a Adapter) mergeSettings(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Factorio server settings: %w", err)
	}
	settings := map[string]any{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("parse Factorio server settings: %w", err)
	}
	if value, ok := a.env("USERNAME"); ok {
		settings[settingsUsernameKey] = value
	}
	if value, ok := a.env("TOKEN"); ok {
		settings[settingsTokenKey] = value
	}
	if value, ok := a.env("GAME_PASSWORD"); ok {
		settings[settingsPasswordKey] = value
	}
	rendered, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("render Factorio server settings: %w", err)
	}
	if err := os.WriteFile(path, append(rendered, '\n'), 0o600); err != nil {
		return fmt.Errorf("write Factorio server settings: %w", err)
	}
	return nil
}

func writeConfigINI(path, dataDir string) error {
	contents := "[path]\nwrite-data=" + dataDir + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write Factorio config.ini: %w", err)
	}
	return nil
}

func selectHostedSave(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read hosted Factorio saves: %w", err)
	}
	type candidate struct {
		name     string
		modified time.Time
		info     os.FileInfo
	}
	var selected candidate
	var found bool
	var tied bool
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".zip") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if !found || info.ModTime().After(selected.modified) {
			selected = candidate{name: entry.Name(), modified: info.ModTime(), info: info}
			found, tied = true, false
			continue
		}
		if info.ModTime().Equal(selected.modified) {
			tied = true
		}
	}
	if !found {
		return "", fmt.Errorf("no hosted Factorio save archive was found")
	}
	if tied {
		return "", fmt.Errorf("hosted Factorio save selection is ambiguous")
	}
	return selected.name, nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
