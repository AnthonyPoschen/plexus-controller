package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	GameID                   = "factorio"
	SchemaVersion            = "factorio/v1"
	SecretSchemaVersion      = "factorio-secrets/v1"
	SecretDataKey            = "secrets.json"
	SecretSchemaAnnotation   = "plexus.gg/secret-schema-version"
	SecretRevisionAnnotation = "plexus.gg/secret-revision"

	minimumNameLength         = 1
	maximumNameLength         = 100
	maximumDescriptionLength  = 500
	maximumTags               = 20
	maximumPlayers            = 65535
	minimumAutosaveMinutes    = 1
	maximumAutosaveMinutes    = 1440
	minimumAutosaveSlots      = 1
	maximumAutosaveSlots      = 100
	maximumAFKMinutes         = 10080
	maximumUsernameLength     = 100
	maximumTokenLength        = 256
	maximumGamePasswordLength = 128
	minimumRCONPasswordLength = 16
	maximumRCONPasswordLength = 128
)

type Configuration struct {
	Name                    string     `json:"name"`
	Description             string     `json:"description,omitempty"`
	Tags                    []string   `json:"tags,omitempty"`
	MaxPlayers              int        `json:"maxPlayers"`
	Visibility              Visibility `json:"visibility"`
	RequireUserVerification bool       `json:"requireUserVerification"`
	AllowCommands           string     `json:"allowCommands"`
	Autosave                Autosave   `json:"autosave"`
	AFKAutokickMinutes      int        `json:"afkAutokickMinutes"`
	AutoPause               bool       `json:"autoPause"`
	OnlyAdminsCanPause      bool       `json:"onlyAdminsCanPause"`
	AutosaveOnlyOnServer    bool       `json:"autosaveOnlyOnServer"`
	NonBlockingSaving       bool       `json:"nonBlockingSaving"`
}

type Visibility struct {
	Public bool `json:"public"`
	LAN    bool `json:"lan"`
}

type Autosave struct {
	IntervalMinutes int `json:"intervalMinutes"`
	Slots           int `json:"slots"`
}

type Secrets struct {
	Username     string `json:"username,omitempty"`
	Token        string `json:"token,omitempty"`
	GamePassword string `json:"gamePassword,omitempty"`
	RCONPassword string `json:"rconPassword"`
}

func DefaultConfiguration() Configuration {
	return Configuration{
		Name:                    "Plexus Factorio Server",
		MaxPlayers:              0,
		Visibility:              Visibility{Public: true, LAN: true},
		RequireUserVerification: true,
		AllowCommands:           "admins-only",
		Autosave:                Autosave{IntervalMinutes: 10, Slots: 5},
		AutoPause:               true,
		OnlyAdminsCanPause:      true,
		AutosaveOnlyOnServer:    true,
	}
}

// DecodeConfiguration applies adapter defaults, rejects unknown JSON fields,
// and validates values before the configuration reaches reconciliation.
func DecodeConfiguration(data []byte) (Configuration, error) {
	configuration := DefaultConfiguration()
	if err := strictDecode(data, &configuration); err != nil {
		return Configuration{}, fmt.Errorf("decode Factorio configuration: %w", err)
	}
	if err := configuration.Validate(); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

// DecodeSecrets rejects unknown secret keys and validates the versioned
// setup-scoped secret structure before it is used by the adapter.
func DecodeSecrets(data []byte) (Secrets, error) {
	var secrets Secrets
	if err := strictDecode(data, &secrets); err != nil {
		return Secrets{}, fmt.Errorf("decode Factorio secrets: %w", err)
	}
	if err := secrets.Validate(); err != nil {
		return Secrets{}, err
	}
	return secrets, nil
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func (configuration Configuration) Validate() error {
	if strings.TrimSpace(configuration.Name) == "" || len(configuration.Name) > maximumNameLength {
		return fmt.Errorf("Factorio name must contain %d to %d characters", minimumNameLength, maximumNameLength)
	}
	if len(configuration.Description) > maximumDescriptionLength {
		return fmt.Errorf("Factorio description must not exceed %d characters", maximumDescriptionLength)
	}
	if len(configuration.Tags) > maximumTags {
		return fmt.Errorf("Factorio tags must not contain more than %d values", maximumTags)
	}
	if configuration.MaxPlayers < 0 || configuration.MaxPlayers > maximumPlayers {
		return fmt.Errorf("Factorio maxPlayers must be between 0 and %d", maximumPlayers)
	}
	if configuration.AllowCommands != "admins-only" && configuration.AllowCommands != "true" && configuration.AllowCommands != "false" {
		return fmt.Errorf("Factorio allowCommands must be admins-only, true, or false")
	}
	if configuration.Autosave.IntervalMinutes < minimumAutosaveMinutes || configuration.Autosave.IntervalMinutes > maximumAutosaveMinutes {
		return fmt.Errorf("Factorio autosave.intervalMinutes must be between %d and %d", minimumAutosaveMinutes, maximumAutosaveMinutes)
	}
	if configuration.Autosave.Slots < minimumAutosaveSlots || configuration.Autosave.Slots > maximumAutosaveSlots {
		return fmt.Errorf("Factorio autosave.slots must be between %d and %d", minimumAutosaveSlots, maximumAutosaveSlots)
	}
	if configuration.AFKAutokickMinutes < 0 || configuration.AFKAutokickMinutes > maximumAFKMinutes {
		return fmt.Errorf("Factorio afkAutokickMinutes must be between 0 and %d", maximumAFKMinutes)
	}
	return nil
}

func (secrets Secrets) Validate() error {
	if (secrets.Username == "") != (secrets.Token == "") {
		return fmt.Errorf("Factorio username and token must be provided together")
	}
	if len(secrets.Username) > maximumUsernameLength || len(secrets.Token) > maximumTokenLength {
		return fmt.Errorf("Factorio account credentials exceed the supported length")
	}
	if len(secrets.GamePassword) > maximumGamePasswordLength {
		return fmt.Errorf("Factorio gamePassword must not exceed %d characters", maximumGamePasswordLength)
	}
	if len(secrets.RCONPassword) < minimumRCONPasswordLength || len(secrets.RCONPassword) > maximumRCONPasswordLength {
		return fmt.Errorf("Factorio rconPassword must contain %d to %d characters", minimumRCONPasswordLength, maximumRCONPasswordLength)
	}
	return nil
}
