package factorio

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	factoriov1 "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

func TestCommandBootsLatestHostedSaveWithMergedSecrets(t *testing.T) {
	paths := testPaths(t)
	older := filepath.Join(paths.SavesDir, "older.zip")
	newer := filepath.Join(paths.SavesDir, "newer.zip")
	if err := os.WriteFile(older, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, paths.SettingsFile, `{"name":"Copper Works","max_players":4}`)

	adapter := Adapter{Paths: paths, LookupEnv: mapEnv(map[string]string{
		"RCON_PASSWORD": "supersecretpassword",
		"USERNAME":      "copper",
		"TOKEN":         "token-value",
		"GAME_PASSWORD": "join-me",
	})}
	cmd, err := adapter.Command(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !containsArgs(cmd.Args, "--start-server", newer) {
		t.Fatalf("start-server = %s", args)
	}
	if !containsArgs(cmd.Args, "--server-settings", paths.SettingsFile) || !containsArgs(cmd.Args, "--mod-directory", paths.ModsDir) {
		t.Fatalf("settings/mod args = %s", args)
	}
	if !containsArgs(cmd.Args, "--rcon-port", "27015") || !containsArgs(cmd.Args, "--rcon-password", "supersecretpassword") {
		t.Fatalf("rcon args = %s", args)
	}
	if cmd.Dir != paths.DataDir {
		t.Fatalf("working directory = %q", cmd.Dir)
	}

	raw, err := os.ReadFile(paths.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["name"] != "Copper Works" || settings["username"] != "copper" || settings["token"] != "token-value" || settings["game_password"] != "join-me" {
		t.Fatalf("merged settings = %s", raw)
	}
	config, err := os.ReadFile(paths.ConfigINI)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "write-data="+paths.DataDir) {
		t.Fatalf("config.ini = %s", config)
	}
}

func TestCommandRequiresHostedSaveAndRCONPassword(t *testing.T) {
	paths := testPaths(t)
	writeSettings(t, paths.SettingsFile, `{"name":"Copper Works"}`)
	adapter := Adapter{Paths: paths, LookupEnv: mapEnv(map[string]string{"RCON_PASSWORD": "supersecretpassword"})}
	if _, err := adapter.Command(context.Background()); err == nil || !strings.Contains(err.Error(), "no hosted Factorio save") {
		t.Fatalf("missing save = %v", err)
	}

	if err := os.WriteFile(filepath.Join(paths.SavesDir, "world.zip"), []byte("save"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter.LookupEnv = mapEnv(nil)
	if _, err := adapter.Command(context.Background()); err == nil || !strings.Contains(err.Error(), "RCON_PASSWORD") {
		t.Fatalf("missing rcon password = %v", err)
	}
}

func TestCommandRejectsAmbiguousSaveSelection(t *testing.T) {
	paths := testPaths(t)
	writeSettings(t, paths.SettingsFile, `{"name":"Copper Works"}`)
	stamp := time.Unix(300, 0)
	for _, name := range []string{"a.zip", "b.zip"} {
		path := filepath.Join(paths.SavesDir, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	adapter := Adapter{Paths: paths, LookupEnv: mapEnv(map[string]string{"RCON_PASSWORD": "supersecretpassword"})}
	if _, err := adapter.Command(context.Background()); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous save = %v", err)
	}
}

func TestGracefulStopSendsAdapterQuit(t *testing.T) {
	received := make(chan string, 1)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	go serveFakeRCON(t, listener, "supersecretpassword", received)

	adapter := Adapter{
		Paths:     Paths{RCONPort: port},
		LookupEnv: mapEnv(map[string]string{"RCON_PASSWORD": "supersecretpassword"}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := adapter.GracefulStop(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-received:
		if command != factoriov1.Schema().Shutdown.Command {
			t.Fatalf("rcon command = %q", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rcon command was not received")
	}
}

func TestGracePeriodMatchesAdapterShutdownPolicy(t *testing.T) {
	if got := (Adapter{}).GracePeriod(); got != time.Duration(factoriov1.Schema().Shutdown.TimeoutSeconds)*time.Second {
		t.Fatalf("grace period = %s", got)
	}
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	paths := Paths{
		Binary:       filepath.Join(root, "factorio-bin"),
		DataDir:      root,
		SavesDir:     filepath.Join(root, "saves"),
		ModsDir:      filepath.Join(root, "mods"),
		SettingsFile: filepath.Join(root, "config", factoriov1.ConfigFileName),
		ConfigINI:    filepath.Join(root, "config", "config.ini"),
		GamePort:     defaultGamePort,
		RCONPort:     defaultRCONPort,
	}
	for _, dir := range []string{paths.SavesDir, paths.ModsDir, filepath.Dir(paths.SettingsFile)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func writeSettings(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mapEnv(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func containsArgs(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func serveFakeRCON(t *testing.T, listener net.Listener, password string, received chan<- string) {
	t.Helper()
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	id, kind, body, err := readRCONPacket(conn)
	if err != nil {
		t.Errorf("auth packet: %v", err)
		return
	}
	if kind != rconAuth || body != password {
		_ = writeRCONPacket(conn, -1, rconAuthResponse, "")
		return
	}
	if err := writeRCONPacket(conn, id, rconAuthResponse, ""); err != nil {
		t.Errorf("auth response: %v", err)
		return
	}
	_, kind, body, err = readRCONPacket(conn)
	if err != nil {
		t.Errorf("exec packet: %v", err)
		return
	}
	if kind != rconExec {
		t.Errorf("exec kind = %d", kind)
		return
	}
	received <- body
}
