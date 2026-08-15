package v1

import "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"

func integer(value int) *int { return &value }

func constraints(value model.Constraints) *model.Constraints { return &value }

// Schema returns the public Project Zomboid v1 management contract. The
// returned value is newly allocated so callers cannot mutate later responses.
func Schema() model.ManagementSchema {
	defaults := DefaultConfiguration()
	return model.ManagementSchema{
		GameID:      GameID,
		Version:     SchemaVersion,
		DisplayName: "Project Zomboid",
		Configuration: model.ConfigurationSchema{Sections: []model.Section{
			{
				ID: "identity", Title: "Server details",
				Fields: []model.Field{
					{Path: "name", Label: "Server name", Description: "Name shown to players joining this hosted world.", Type: model.FieldTypeString, Required: true, Default: defaults.Name, Constraints: constraints(model.Constraints{MinLength: integer(minimumNameLength), MaxLength: integer(maximumNameLength)})},
					{Path: "description", Label: "Description", Description: "Optional public description shown with the hosted world.", Type: model.FieldTypeString, Required: false, Constraints: constraints(model.Constraints{MaxLength: integer(maximumDescriptionLength)})},
				},
			},
			{
				ID: "access", Title: "Access and world rules",
				Fields: []model.Field{
					{Path: "maxPlayers", Label: "Maximum players", Description: "Maximum concurrent survivors on this hosted world.", Type: model.FieldTypeInteger, Required: true, Default: defaults.MaxPlayers, Constraints: constraints(model.Constraints{Minimum: integer(minimumPlayers), Maximum: integer(maximumPlayers)})},
					{Path: "public", Label: "Open to approved players", Description: "Allow players who are not already allowlisted to attempt to join.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.Public},
					{Path: "pvp", Label: "Player versus player", Description: "Allow survivors to damage each other.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.PVP},
					{Path: "pauseEmpty", Label: "Pause when empty", Description: "Pause the world when no players are connected.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.PauseEmpty},
				},
			},
		}},
		Secrets: model.SecretSchema{
			Version: SecretSchemaVersion,
			Fields: []model.Field{
				{Path: "adminPassword", Label: "Admin password", Description: "Password for the hosted in-game admin account.", Type: model.FieldTypeSecret, Required: true, Sensitive: true, Constraints: constraints(model.Constraints{MinLength: integer(minimumPasswordLength), MaxLength: integer(maximumPasswordLength)})},
				{Path: "serverPassword", Label: "Join password", Description: "Optional password players must enter before joining.", Type: model.FieldTypeSecret, Sensitive: true, Constraints: constraints(model.Constraints{MinLength: integer(minimumPasswordLength), MaxLength: integer(maximumPasswordLength)})},
				{Path: "rconPassword", Label: "RCON password", Description: "Platform-generated credential reserved for future remote administration.", Type: model.FieldTypeSecret, Required: true, Sensitive: true, Generated: true, Constraints: constraints(model.Constraints{MinLength: integer(minimumRCONPasswordLength), MaxLength: integer(maximumRCONPasswordLength)})},
			},
		},
		Capabilities: []model.Capability{
			{ID: "configuration", Released: true, Description: "Tailored server settings backed by the typed Project Zomboid configuration."},
			{ID: "mods", Released: false, Description: "Workshop mod management is not released for Project Zomboid."},
			{ID: "saves", Released: false, Description: "Hosted save import and export are not released for Project Zomboid."},
			{ID: "console", Released: false, Description: "Interactive Project Zomboid administration is not released."},
			{ID: "logs", Released: true, Description: "Read-only live container output."},
		},
		Runtime: model.RuntimePolicy{Channels: []model.RuntimeChannel{
			{ID: "container-stdout", Label: "Server output", Interaction: model.InteractionReadOnly, Protocol: "container-stdout", Released: true},
		}},
		Shutdown: model.ShutdownPolicy{Strategy: "process-signal", TimeoutSeconds: 120, ForceSupported: true},
		Saves:    model.SavePolicy{RequiresStopped: true, DestructiveImport: true, LeavesServerStopped: true},
		Mods:     model.ModProviderPolicy{RequiresStopped: true, AutomaticRestart: false},
	}
}
