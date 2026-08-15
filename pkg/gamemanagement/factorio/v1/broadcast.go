package v1

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	BroadcastChannelChat            = "chat"
	BroadcastProtocolRCON           = "rcon"
	MaximumBroadcastMessageRunes    = 240
	broadcastLuaLongStringCloser    = "]==]"
	broadcastAttributionPrefix      = "Plexus"
	DefaultModUpdateNotice          = "The server will restart shortly to apply pending mod updates."
	DefaultPlannedMaintenanceNotice = "The server will restart shortly for planned maintenance."
)

// DefaultMaintenanceNotice is the safe customer-editable body shown before
// Restart. Callers still have to start Restart separately.
func DefaultMaintenanceNotice(updates []string) string {
	cleaned := make([]string, 0, len(updates))
	for _, update := range updates {
		update = strings.TrimSpace(update)
		if update != "" {
			cleaned = append(cleaned, update)
		}
	}
	switch len(cleaned) {
	case 0:
		return DefaultPlannedMaintenanceNotice
	case 1:
		return "The server will restart shortly to apply " + cleaned[0] + "."
	default:
		return DefaultModUpdateNotice + " " + strings.Join(cleaned, ", ") + "."
	}
}

// BroadcastAttribution is the visible prefix players see. The owner email is
// included when present so the notice is attributable.
func BroadcastAttribution(actorEmail string) string {
	actorEmail = strings.TrimSpace(actorEmail)
	if actorEmail == "" {
		return broadcastAttributionPrefix
	}
	return broadcastAttributionPrefix + " · " + actorEmail
}

// FormatAttributedNotice joins the platform prefix with a validated customer
// message. The result is what connected players should see.
func FormatAttributedNotice(actorEmail string, message string) (string, error) {
	message, err := NormalizeBroadcastMessage(message, nil)
	if err != nil {
		return "", err
	}
	return BroadcastAttribution(actorEmail) + ": " + message, nil
}

// NormalizeBroadcastMessage trims and validates a customer-supplied notice. An
// empty value uses the adapter default derived from pending updates.
func NormalizeBroadcastMessage(message string, updates []string) (string, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = DefaultMaintenanceNotice(updates)
	}
	if err := validateBroadcastText(message, MaximumBroadcastMessageRunes); err != nil {
		return "", err
	}
	return message, nil
}

// ChatBroadcastCommand renders a Factorio RCON chat print that cannot be used
// as an arbitrary Lua or console command. Delivery never restarts the Server.
func ChatBroadcastCommand(notice string) (string, error) {
	if err := validateBroadcastText(notice, MaximumBroadcastMessageRunes+80); err != nil {
		return "", err
	}
	return "/silent-command game.print([==[" + notice + "]==])", nil
}

func validateBroadcastText(value string, maximumRunes int) error {
	if value == "" || utf8.RuneCountInString(value) > maximumRunes {
		return fmt.Errorf("maintenance notice is empty or too long")
	}
	if strings.Contains(value, "\x00") || strings.Contains(value, broadcastLuaLongStringCloser) {
		return fmt.Errorf("maintenance notice contains unsafe characters")
	}
	for _, runeValue := range value {
		if runeValue < 32 && runeValue != '\t' {
			return fmt.Errorf("maintenance notice cannot include control characters")
		}
	}
	return nil
}
