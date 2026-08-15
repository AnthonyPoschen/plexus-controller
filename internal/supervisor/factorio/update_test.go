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

func TestSteamcmdUpdaterRejectsPinnedPatchAndRerunsOnLaterBoot(t *testing.T) {
	t.Parallel()
	var calls int
	updater := SteamcmdUpdater{
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
