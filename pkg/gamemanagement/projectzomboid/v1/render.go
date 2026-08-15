package v1

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	ConfigFileName = "server.ini"
	AdminUsername  = "admin"
)

// RenderConfigFiles turns decoded non-sensitive values into the hosted INI
// consumed by the Project Zomboid dedicated server.
func RenderConfigFiles(configuration Configuration) (map[string]string, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	var builder strings.Builder
	if err := writeINI(&builder, "PublicName", configuration.Name); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "PublicDescription", configuration.Description); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "MaxPlayers", strconv.Itoa(configuration.MaxPlayers)); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "Open", strconv.FormatBool(configuration.Public)); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "PVP", strconv.FormatBool(configuration.PVP)); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "PauseEmpty", strconv.FormatBool(configuration.PauseEmpty)); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "DefaultPort", "16261"); err != nil {
		return nil, err
	}
	if err := writeINI(&builder, "UDPPort", "16262"); err != nil {
		return nil, err
	}
	return map[string]string{ConfigFileName: builder.String()}, nil
}

// RuntimeSecretEnv copies decoded secrets into environment values. Plaintext
// never belongs in a ConfigMap or status.
func RuntimeSecretEnv(secrets Secrets) (map[string][]byte, error) {
	if err := secrets.Validate(); err != nil {
		return nil, err
	}
	return map[string][]byte{
		"ADMIN_PASSWORD":  []byte(secrets.AdminPassword),
		"SERVER_PASSWORD": []byte(secrets.ServerPassword),
		"RCON_PASSWORD":   []byte(secrets.RCONPassword),
	}, nil
}

// ConfigurationEnv maps tailored settings onto the dedicated-server image
// variables that overwrite the matching INI keys on start.
func ConfigurationEnv(configuration Configuration) (map[string]string, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	return map[string]string{
		"SERVER_NAME":    ConfigIdentity,
		"MAX_PLAYERS":    strconv.Itoa(configuration.MaxPlayers),
		"PUBLIC_SERVER":  strconv.FormatBool(configuration.Public),
		"PAUSE_ON_EMPTY": strconv.FormatBool(configuration.PauseEmpty),
	}, nil
}

func writeINI(builder *strings.Builder, key string, value string) error {
	if strings.ContainsAny(key, "=\n\r") || strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("Project Zomboid configuration cannot render INI key %q", key)
	}
	builder.WriteString(key)
	builder.WriteByte('=')
	builder.WriteString(value)
	builder.WriteByte('\n')
	return nil
}
