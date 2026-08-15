package v1

import (
	"strings"
	"testing"

	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"
)

func TestFactorioBroadcastPolicyDeclaresChatAndNeverRestarts(t *testing.T) {
	schema := Schema()
	if schema.Broadcast.Supported() == false || schema.Broadcast.Channel != model.BroadcastChannelChat || schema.Broadcast.Protocol != BroadcastProtocolRCON || schema.Broadcast.AutomaticRestart {
		t.Fatalf("Factorio broadcast policy drifted: %#v", schema.Broadcast)
	}
	if schema.Broadcast.MaximumMessageRunes != MaximumBroadcastMessageRunes {
		t.Fatalf("broadcast message limit = %d", schema.Broadcast.MaximumMessageRunes)
	}
	found := false
	for _, capability := range schema.Capabilities {
		if capability.ID == "broadcast" && capability.Released {
			found = true
			break
		}
	}
	if found == false {
		t.Fatal("Factorio schema omitted the released broadcast capability")
	}
}

func TestChatBroadcastCommandAttributesAndRejectsInjection(t *testing.T) {
	notice, err := FormatAttributedNotice("owner@example.com", "Restarting soon for a mod update.")
	if err != nil {
		t.Fatal(err)
	}
	if notice != "Plexus · owner@example.com: Restarting soon for a mod update." {
		t.Fatalf("attributed notice = %q", notice)
	}
	command, err := ChatBroadcastCommand(notice)
	if err != nil {
		t.Fatal(err)
	}
	if command != "/silent-command game.print([==[Plexus · owner@example.com: Restarting soon for a mod update.]==])" {
		t.Fatalf("broadcast command = %q", command)
	}

	if _, err := FormatAttributedNotice("owner@example.com", "break ]==] out"); err == nil {
		t.Fatal("expected Lua long-string closer to be rejected")
	}
	if _, err := NormalizeBroadcastMessage(" \n\t ", nil); err != nil {
		t.Fatalf("empty input should use the default notice: %v", err)
	}
	if DefaultMaintenanceNotice([]string{"tiny-mod 1.2.4"}) != "The server will restart shortly to apply tiny-mod 1.2.4." {
		t.Fatalf("single-update default = %q", DefaultMaintenanceNotice([]string{"tiny-mod 1.2.4"}))
	}
	if _, err := NormalizeBroadcastMessage(strings.Repeat("x", MaximumBroadcastMessageRunes+1), nil); err == nil {
		t.Fatal("expected an oversized notice to be rejected")
	}
}

func TestUnsupportedBroadcastPolicyIsOmitted(t *testing.T) {
	var policy model.BroadcastPolicy
	if policy.Supported() {
		t.Fatal("zero broadcast policy must not claim support")
	}
}
