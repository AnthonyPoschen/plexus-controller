package v1

import (
	"encoding/json"
	"fmt"
)

const ConfigFileName = "server-settings.json"

// RenderConfigFiles turns decoded non-sensitive values into the Factorio
// server-settings.json document mounted from a ConfigMap.
func RenderConfigFiles(configuration Configuration) (map[string]string, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	tags := configuration.Tags
	if tags == nil {
		tags = []string{}
	}
	settings := struct {
		Name                    string     `json:"name"`
		Description             string     `json:"description"`
		Tags                    []string   `json:"tags"`
		MaxPlayers              int        `json:"max_players"`
		Visibility              Visibility `json:"visibility"`
		RequireUserVerification bool       `json:"require_user_verification"`
		AllowCommands           string     `json:"allow_commands"`
		AutosaveInterval        int        `json:"autosave_interval"`
		AutosaveSlots           int        `json:"autosave_slots"`
		AFKAutokickInterval     int        `json:"afk_autokick_interval"`
		AutoPause               bool       `json:"auto_pause"`
		OnlyAdminsCanPause      bool       `json:"only_admins_can_pause_the_game"`
		AutosaveOnlyOnServer    bool       `json:"autosave_only_on_server"`
		NonBlockingSaving       bool       `json:"non_blocking_saving"`
	}{
		Name: configuration.Name, Description: configuration.Description, Tags: tags,
		MaxPlayers: configuration.MaxPlayers, Visibility: configuration.Visibility,
		RequireUserVerification: configuration.RequireUserVerification, AllowCommands: configuration.AllowCommands,
		AutosaveInterval: configuration.Autosave.IntervalMinutes, AutosaveSlots: configuration.Autosave.Slots,
		AFKAutokickInterval: configuration.AFKAutokickMinutes, AutoPause: configuration.AutoPause,
		OnlyAdminsCanPause: configuration.OnlyAdminsCanPause, AutosaveOnlyOnServer: configuration.AutosaveOnlyOnServer,
		NonBlockingSaving: configuration.NonBlockingSaving,
	}
	rendered, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render Factorio server settings: %w", err)
	}
	return map[string]string{ConfigFileName: string(append(rendered, '\n'))}, nil
}

// RuntimeSecretEnv copies decoded secrets into environment values. Plaintext
// never belongs in a ConfigMap or status.
func RuntimeSecretEnv(secrets Secrets) (map[string][]byte, error) {
	if err := secrets.Validate(); err != nil {
		return nil, err
	}
	return map[string][]byte{
		"USERNAME":      []byte(secrets.Username),
		"TOKEN":         []byte(secrets.Token),
		"GAME_PASSWORD": []byte(secrets.GamePassword),
		"RCON_PASSWORD": []byte(secrets.RCONPassword),
	}, nil
}
