package v1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	GameID                   = "project-zomboid"
	SchemaVersion            = "project-zomboid/v1"
	SecretSchemaVersion      = "project-zomboid-secrets/v1"
	SecretDataKey            = "secrets.json"
	SecretSchemaAnnotation   = "plexus.gg/secret-schema-version"
	SecretRevisionAnnotation = "plexus.gg/secret-revision"

	SupportedImageTag = "2.5.0"
	ConfigIdentity    = "plexus"

	minimumNameLength         = 1
	maximumNameLength         = 64
	maximumDescriptionLength  = 500
	minimumPlayers            = 1
	maximumPlayers            = 64
	minimumPasswordLength     = 8
	maximumPasswordLength     = 64
	minimumRCONPasswordLength = 16
	maximumRCONPasswordLength = 64
)

var hostedNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$`)
var secretValuePattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

type Configuration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MaxPlayers  int    `json:"maxPlayers"`
	Public      bool   `json:"public"`
	PVP         bool   `json:"pvp"`
	PauseEmpty  bool   `json:"pauseEmpty"`
}

type Secrets struct {
	AdminPassword  string `json:"adminPassword"`
	ServerPassword string `json:"serverPassword,omitempty"`
	RCONPassword   string `json:"rconPassword"`
}

func DefaultConfiguration() Configuration {
	return Configuration{
		Name:       "Plexus Zomboid",
		MaxPlayers: 16,
		Public:     false,
		PVP:        true,
		PauseEmpty: true,
	}
}

// DecodeConfiguration applies adapter defaults, rejects unknown JSON fields,
// and validates values before the configuration reaches reconciliation.
func DecodeConfiguration(data []byte) (Configuration, error) {
	configuration := DefaultConfiguration()
	if err := strictDecode(data, &configuration); err != nil {
		return Configuration{}, fmt.Errorf("decode Project Zomboid configuration: %w", err)
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
		return Secrets{}, fmt.Errorf("decode Project Zomboid secrets: %w", err)
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
	name := strings.TrimSpace(configuration.Name)
	if name == "" || utf8.RuneCountInString(name) > maximumNameLength || !hostedNamePattern.MatchString(name) {
		return fmt.Errorf("Project Zomboid name must be %d to %d letters, numbers, spaces, underscores, or hyphens", minimumNameLength, maximumNameLength)
	}
	if utf8.RuneCountInString(configuration.Description) > maximumDescriptionLength || strings.ContainsAny(configuration.Description, "\n\r") {
		return fmt.Errorf("Project Zomboid description must not exceed %d characters or contain line breaks", maximumDescriptionLength)
	}
	if configuration.MaxPlayers < minimumPlayers || configuration.MaxPlayers > maximumPlayers {
		return fmt.Errorf("Project Zomboid maxPlayers must be between %d and %d", minimumPlayers, maximumPlayers)
	}
	return nil
}

func (secrets Secrets) Validate() error {
	if err := validateSecretValue("adminPassword", secrets.AdminPassword, minimumPasswordLength, maximumPasswordLength, true); err != nil {
		return err
	}
	if secrets.ServerPassword != "" {
		if err := validateSecretValue("serverPassword", secrets.ServerPassword, minimumPasswordLength, maximumPasswordLength, false); err != nil {
			return err
		}
	}
	if err := validateSecretValue("rconPassword", secrets.RCONPassword, minimumRCONPasswordLength, maximumRCONPasswordLength, true); err != nil {
		return err
	}
	return nil
}

func validateSecretValue(name string, value string, minimum int, maximum int, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("Project Zomboid %s is required", name)
		}
		return nil
	}
	if !secretValuePattern.MatchString(value) || len(value) < minimum || len(value) > maximum {
		return fmt.Errorf("Project Zomboid %s must contain %d to %d letters or numbers", name, minimum, maximum)
	}
	return nil
}
