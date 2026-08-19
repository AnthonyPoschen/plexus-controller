package factorio

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	factoriov1 "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

func TestSteamcmdUpdaterAppliesSelectedChannelWithoutTouchingWorldData(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		channel string
		beta    bool
	}{
		{channel: factoriov1.ChannelStable},
		{channel: factoriov1.ChannelExperimental, beta: true},
	} {
		t.Run(test.channel, func(t *testing.T) {
			var got []string
			updater := SteamcmdUpdater{
				Steamcmd:   "/opt/steamcmd/steamcmd.sh",
				InstallDir: "/opt/factorio",
				Username:   "plexus-steam",
				Password:   "platform-pass",
				Command: func(_ context.Context, name string, args ...string) *exec.Cmd {
					got = append([]string{name}, args...)
					return exec.Command("true")
				},
			}
			if err := updater.Update(context.Background(), test.channel); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(got, " ")
			if got[0] != "/opt/steamcmd/steamcmd.sh" {
				t.Fatalf("steamcmd = %q", got[0])
			}
			if !containsArgs(got, "+force_install_dir", "/opt/factorio") || !containsArgs(got, "+app_update", steamAppID) {
				t.Fatalf("update args = %s", joined)
			}
			if !containsArgs(got, "+login", "plexus-steam") || !containsArgs(got, "plexus-steam", "platform-pass") {
				t.Fatalf("credentialed login args = %s", joined)
			}
			if containsArg(got, "anonymous") {
				t.Fatalf("steamcmd must not log in anonymously when credentials are present: %s", joined)
			}
			if strings.Contains(joined, "/factorio/saves") || strings.Contains(joined, "/factorio/mods") || strings.Contains(joined, defaultDataDir+"/saves") {
				t.Fatalf("updater must not touch hosted saves or mods: %s", joined)
			}
			hasBeta := containsArgs(got, "-beta", steamExperimental)
			if hasBeta != test.beta {
				t.Fatalf("experimental beta flag = %t, want %t in %s", hasBeta, test.beta, joined)
			}
		})
	}
}

func TestSteamcmdUpdaterLogsInWithMountedPlatformCredentials(t *testing.T) {
	t.Parallel()
	var got []string
	updater := SteamcmdUpdater{
		LookupEnv: mapEnv(map[string]string{
			steamUsernameEnv: "plexus-steam",
			steamPasswordEnv: "platform-pass",
			"USERNAME":       "factorio-account",
			"TOKEN":          "factorio-token",
		}),
		Command: func(_ context.Context, name string, args ...string) *exec.Cmd {
			got = append([]string{name}, args...)
			return exec.Command("true")
		},
	}
	if err := updater.Update(context.Background(), factoriov1.ChannelExperimental); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !containsArgs(got, "+login", "plexus-steam") || !containsArgs(got, "plexus-steam", "platform-pass") {
		t.Fatalf("platform login args = %s", joined)
	}
	if containsArg(got, "anonymous") || containsArg(got, "factorio-account") || containsArg(got, "factorio-token") {
		t.Fatalf("login used customer Factorio.com credentials or anonymous: %s", joined)
	}
	if !containsArgs(got, "+app_update", steamAppID) || !containsArgs(got, "-beta", steamExperimental) {
		t.Fatalf("experimental update args = %s", joined)
	}
}

func TestSteamcmdUpdaterDoesNotLoginAnonymousWhenCredentialsAreMissing(t *testing.T) {
	t.Parallel()
	var called bool
	updater := SteamcmdUpdater{
		LookupEnv: mapEnv(map[string]string{
			"USERNAME": "factorio-account",
			"TOKEN":    "factorio-token",
		}),
		Command: func(_ context.Context, _ string, _ ...string) *exec.Cmd {
			called = true
			return exec.Command("true")
		},
	}
	err := updater.Update(context.Background(), factoriov1.ChannelStable)
	if err == nil || !strings.Contains(err.Error(), "platform steamcmd credentials") {
		t.Fatalf("missing secret = %v", err)
	}
	if called {
		t.Fatal("steamcmd must not run without platform credentials")
	}
}

func TestSteamcmdUpdaterRejectsPinnedPatchAndRerunsOnLaterBoot(t *testing.T) {
	t.Parallel()
	var calls int
	updater := SteamcmdUpdater{
		Username: "plexus-steam",
		Password: "platform-pass",
		Command: func(_ context.Context, _ string, _ ...string) *exec.Cmd {
			calls++
			return exec.Command("true")
		},
	}
	if err := updater.Update(context.Background(), "2.0.77"); err == nil {
		t.Fatal("expected a pinned patch to be rejected")
	}
	if calls != 0 {
		t.Fatalf("rejected channel still invoked steamcmd %d times", calls)
	}
	if err := updater.Update(context.Background(), factoriov1.ChannelStable); err != nil {
		t.Fatal(err)
	}
	if err := updater.Update(context.Background(), factoriov1.ChannelStable); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("later boot must apply channel diffs again, calls=%d", calls)
	}
}

func TestSelectedChannelDefaultsToStable(t *testing.T) {
	t.Parallel()
	channel, err := selectedChannel("", false)
	if err != nil || channel != factoriov1.ChannelStable {
		t.Fatalf("missing env = %q err=%v", channel, err)
	}
	channel, err = selectedChannel("experimental", true)
	if err != nil || channel != factoriov1.ChannelExperimental {
		t.Fatalf("experimental env = %q err=%v", channel, err)
	}
	if _, err := selectedChannel("2.0.77", true); err == nil {
		t.Fatal("expected pinned patch env to be rejected")
	}
}
