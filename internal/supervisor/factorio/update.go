package factorio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	factoriov1 "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	defaultSteamcmd   = "/opt/steamcmd/steamcmd.sh"
	defaultInstallDir = "/opt/factorio"
	steamAppID        = "427520"
	steamPlatform     = "linux"
	steamExperimental = "experimental"
	steamUsernameEnv  = "STEAM_USERNAME"
	steamPasswordEnv  = "STEAM_PASSWORD"
)

// errMissingSteamcmdCredentials is returned when the optional platform
// SteamCMD Secret is not mounted. Paid apps cannot update anonymously.
var errMissingSteamcmdCredentials = errors.New("platform steamcmd credentials are missing")

// GameUpdater updates Factorio dedicated-server files to the latest build of a
// channel. Implementations must not touch hosted saves or mods.
type GameUpdater interface {
	Update(ctx context.Context, channel string) error
}

// SteamcmdUpdater applies the selected channel through SteamCMD into the image
// install directory. The image seed is replaced on every boot so later starts
// pick up newer channel diffs without rebuilding the Plexus image.
// Login uses the platform Steam account from STEAM_USERNAME / STEAM_PASSWORD,
// never anonymous and never the customer Factorio.com USERNAME / TOKEN.
type SteamcmdUpdater struct {
	Steamcmd   string
	InstallDir string
	Username   string
	Password   string
	LookupEnv  func(string) (string, bool)
	Command    func(ctx context.Context, name string, args ...string) *exec.Cmd
	Output     io.Writer
}

func (u SteamcmdUpdater) Update(ctx context.Context, channel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := factoriov1.ValidateChannel(channel); err != nil {
		return err
	}
	username, password, ok := u.credentials()
	if !ok {
		return errMissingSteamcmdCredentials
	}
	steamcmd := u.Steamcmd
	if steamcmd == "" {
		steamcmd = defaultSteamcmd
	}
	installDir := u.InstallDir
	if installDir == "" {
		installDir = defaultInstallDir
	}
	args := []string{
		"+@sSteamCmdForcePlatformType", steamPlatform,
		"+force_install_dir", installDir,
		"+login", username, password,
		"+app_update", steamAppID,
	}
	if channel == factoriov1.ChannelExperimental {
		args = append(args, "-beta", steamExperimental)
	}
	args = append(args, "validate", "+quit")

	cmd := u.command(ctx, steamcmd, args...)
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(steamcmd))
	if cmd.Stdout == nil {
		cmd.Stdout = u.writer()
	}
	if cmd.Stderr == nil {
		cmd.Stderr = u.writer()
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("steamcmd update %s: %w", channel, err)
	}
	return nil
}

func (u SteamcmdUpdater) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if u.Command != nil {
		return u.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (u SteamcmdUpdater) writer() io.Writer {
	if u.Output != nil {
		return u.Output
	}
	return os.Stderr
}

func (u SteamcmdUpdater) credentials() (string, string, bool) {
	username := strings.TrimSpace(u.Username)
	password := u.Password
	if username == "" || strings.TrimSpace(password) == "" {
		username, _ = u.env(steamUsernameEnv)
		password, _ = u.env(steamPasswordEnv)
		username = strings.TrimSpace(username)
	}
	if username == "" || strings.TrimSpace(password) == "" {
		return "", "", false
	}
	return username, password, true
}

func (u SteamcmdUpdater) env(name string) (string, bool) {
	if u.LookupEnv != nil {
		return u.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func selectedChannel(value string, present bool) (string, error) {
	channel := strings.TrimSpace(value)
	if !present || channel == "" {
		return factoriov1.ChannelStable, nil
	}
	if err := factoriov1.ValidateChannel(channel); err != nil {
		return "", err
	}
	return channel, nil
}
