// Package gamemanagement exposes controller-owned game contracts to consumers
// without requiring access to a running controller.
package gamemanagement

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"
	zomboid "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/projectzomboid/v1"
)

const (
	SecretDataKey            = "secrets.json"
	SecretSchemaAnnotation   = "plexus.gg/secret-schema-version"
	SecretRevisionAnnotation = "plexus.gg/secret-revision"
)

func Schema(gameID string) (model.ManagementSchema, bool) {
	switch gameID {
	case factorio.GameID:
		return factorio.Schema(), true
	case zomboid.GameID:
		return zomboid.Schema(), true
	default:
		return model.ManagementSchema{}, false
	}
}

// NormalizeConfiguration decodes, defaults, and re-encodes one adapter
// configuration document so backend and controller share the same validator.
func NormalizeConfiguration(gameID string, data []byte) (json.RawMessage, error) {
	switch gameID {
	case factorio.GameID:
		configuration, err := factorio.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(configuration)
	case zomboid.GameID:
		configuration, err := zomboid.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(configuration)
	default:
		return nil, fmt.Errorf("unsupported game %q", gameID)
	}
}

// NormalizeSecrets decodes and re-encodes one adapter secret document.
func NormalizeSecrets(gameID string, data []byte) (json.RawMessage, error) {
	switch gameID {
	case factorio.GameID:
		secrets, err := factorio.DecodeSecrets(data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(secrets)
	case zomboid.GameID:
		secrets, err := zomboid.DecodeSecrets(data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(secrets)
	default:
		return nil, fmt.Errorf("unsupported game %q", gameID)
	}
}

// ApplySecretPatch merges customer-submitted non-generated secret fields into
// the stored document and fills any missing generated credentials.
func ApplySecretPatch(gameID string, existing []byte, patch map[string]*string) (json.RawMessage, error) {
	schema, ok := Schema(gameID)
	if !ok {
		return nil, fmt.Errorf("unsupported game %q", gameID)
	}

	current := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 && string(bytes.TrimSpace(existing)) != "null" {
		normalized, err := NormalizeSecrets(gameID, existing)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(normalized, &current); err != nil {
			return nil, err
		}
	}

	fields := map[string]model.Field{}
	for _, field := range schema.Secrets.Fields {
		fields[field.Path] = field
	}
	for path := range patch {
		field, known := fields[path]
		if !known || field.Generated {
			return nil, fmt.Errorf("secret field %q cannot be set by the customer", path)
		}
	}
	firstDocument := len(existing) == 0
	for path, field := range fields {
		value, _ := current[path].(string)
		if value != "" {
			continue
		}
		if field.Generated == false && (firstDocument == false || field.Required == false || patch[path] != nil) {
			continue
		}
		generated, err := generateSecretValue(field)
		if err != nil {
			return nil, err
		}
		current[path] = generated
	}
	for path, value := range patch {
		if value == nil || *value == "" {
			delete(current, path)
			continue
		}
		current[path] = *value
	}

	encoded, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	return NormalizeSecrets(gameID, encoded)
}

// RenderConfigFiles returns the adapter-owned config-file map for a decoded
// configuration document.
func RenderConfigFiles(gameID string, data []byte) (map[string]string, error) {
	switch gameID {
	case factorio.GameID:
		configuration, err := factorio.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return factorio.RenderConfigFiles(configuration)
	case zomboid.GameID:
		configuration, err := zomboid.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return zomboid.RenderConfigFiles(configuration)
	default:
		return nil, fmt.Errorf("unsupported game %q", gameID)
	}
}

// RuntimeSecretEnv returns SecretKeyRef environment data for a decoded secret
// document.
func RuntimeSecretEnv(gameID string, data []byte) (map[string][]byte, error) {
	switch gameID {
	case factorio.GameID:
		secrets, err := factorio.DecodeSecrets(data)
		if err != nil {
			return nil, err
		}
		return factorio.RuntimeSecretEnv(secrets)
	case zomboid.GameID:
		secrets, err := zomboid.DecodeSecrets(data)
		if err != nil {
			return nil, err
		}
		return zomboid.RuntimeSecretEnv(secrets)
	default:
		return nil, fmt.Errorf("unsupported game %q", gameID)
	}
}

// ConfigurationEnv returns non-sensitive image environment values derived
// from a decoded configuration document.
func ConfigurationEnv(gameID string, data []byte) (map[string]string, error) {
	switch gameID {
	case factorio.GameID:
		configuration, err := factorio.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return factorio.ConfigurationEnv(configuration)
	case zomboid.GameID:
		configuration, err := zomboid.DecodeConfiguration(data)
		if err != nil {
			return nil, err
		}
		return zomboid.ConfigurationEnv(configuration)
	default:
		return map[string]string{}, nil
	}
}

func generateSecretValue(field model.Field) (string, error) {
	length := 24
	if field.Constraints != nil && field.Constraints.MinLength != nil && *field.Constraints.MinLength > length {
		length = *field.Constraints.MinLength
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s: %w", field.Path, err)
	}
	for index := range raw {
		raw[index] = alphabet[int(raw[index])%len(alphabet)]
	}
	return string(raw), nil
}
