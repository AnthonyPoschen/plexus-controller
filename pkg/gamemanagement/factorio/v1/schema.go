package v1

import "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"

func integer(value int) *int { return &value }

func constraints(value model.Constraints) *model.Constraints { return &value }

// Schema returns the public Factorio v1 management contract. The returned
// value is newly allocated so callers cannot mutate later responses.
func Schema() model.ManagementSchema {
	defaults := DefaultConfiguration()
	return model.ManagementSchema{
		GameID:      GameID,
		Version:     SchemaVersion,
		DisplayName: "Factorio",
		Configuration: model.ConfigurationSchema{Sections: []model.Section{
			{
				ID: "identity", Title: "Server details",
				Fields: []model.Field{
					{Path: "name", Label: "Server name", Description: "Name shown in the Factorio multiplayer browser.", Type: model.FieldTypeString, Required: true, Default: defaults.Name, Constraints: constraints(model.Constraints{MinLength: integer(minimumNameLength), MaxLength: integer(maximumNameLength)})},
					{Path: "description", Label: "Description", Description: "Description shown to players in the multiplayer browser.", Type: model.FieldTypeString, Required: false, Constraints: constraints(model.Constraints{MaxLength: integer(maximumDescriptionLength)})},
					{Path: "tags", Label: "Tags", Description: "Searchable tags shown in the multiplayer browser.", Type: model.FieldTypeStringList, Required: false, Constraints: constraints(model.Constraints{MaxItems: integer(maximumTags)})},
				},
			},
			{
				ID: "access", Title: "Access and visibility",
				Fields: []model.Field{
					{Path: "maxPlayers", Label: "Maximum players", Description: "Maximum concurrent players; 0 allows Factorio's default unlimited player count.", Type: model.FieldTypeInteger, Required: true, Default: defaults.MaxPlayers, Constraints: constraints(model.Constraints{Minimum: integer(0), Maximum: integer(maximumPlayers)})},
					{Path: "visibility.public", Label: "Public listing", Description: "List this server in Factorio's public multiplayer browser.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.Visibility.Public},
					{Path: "visibility.lan", Label: "LAN visibility", Description: "Advertise this server to local-network discovery.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.Visibility.LAN},
					{Path: "requireUserVerification", Label: "Verify player identity", Description: "Require Factorio account verification for joining players.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.RequireUserVerification},
					{Path: "allowCommands", Label: "Console commands", Description: "Choose who may run Lua commands that can alter the save.", Type: model.FieldTypeString, Required: true, Default: defaults.AllowCommands, Options: []model.Option{{Value: "admins-only", Label: "Admins only"}, {Value: "true", Label: "Everyone"}, {Value: "false", Label: "Disabled"}}},
				},
			},
			{
				ID: "saving", Title: "Saving and idle behavior",
				Fields: []model.Field{
					{Path: "autosave.intervalMinutes", Label: "Autosave interval", Description: "Minutes between automatic saves.", Type: model.FieldTypeInteger, Required: true, Default: defaults.Autosave.IntervalMinutes, Constraints: constraints(model.Constraints{Minimum: integer(minimumAutosaveMinutes), Maximum: integer(maximumAutosaveMinutes)})},
					{Path: "autosave.slots", Label: "Autosave slots", Description: "Number of rotating autosaves to retain.", Type: model.FieldTypeInteger, Required: true, Default: defaults.Autosave.Slots, Constraints: constraints(model.Constraints{Minimum: integer(minimumAutosaveSlots), Maximum: integer(maximumAutosaveSlots)})},
					{Path: "afkAutokickMinutes", Label: "AFK auto-kick", Description: "Minutes before an inactive player is removed; 0 disables auto-kick.", Type: model.FieldTypeInteger, Required: true, Default: defaults.AFKAutokickMinutes, Constraints: constraints(model.Constraints{Minimum: integer(0), Maximum: integer(maximumAFKMinutes)})},
					{Path: "autoPause", Label: "Pause when empty", Description: "Pause simulation when no players are connected.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.AutoPause},
					{Path: "onlyAdminsCanPause", Label: "Admin-only pause", Description: "Allow only administrators to pause manually.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.OnlyAdminsCanPause},
					{Path: "autosaveOnlyOnServer", Label: "Server-only autosaves", Description: "Keep multiplayer autosaves on the hosted server.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.AutosaveOnlyOnServer},
					{Path: "nonBlockingSaving", Label: "Non-blocking saves", Description: "Use Factorio's non-blocking save mode when supported by the host.", Type: model.FieldTypeBoolean, Required: true, Default: defaults.NonBlockingSaving},
				},
			},
		}},
		Secrets: model.SecretSchema{
			Version: SecretSchemaVersion,
			Fields: []model.Field{
				{Path: "username", Label: "Factorio username", Description: "Factorio account name used for authenticated public listing and mod access.", Type: model.FieldTypeSecret, Sensitive: true, Constraints: constraints(model.Constraints{MaxLength: integer(maximumUsernameLength)})},
				{Path: "token", Label: "Factorio token", Description: "Factorio account token paired with the username.", Type: model.FieldTypeSecret, Sensitive: true, Constraints: constraints(model.Constraints{MaxLength: integer(maximumTokenLength)})},
				{Path: "gamePassword", Label: "Game password", Description: "Optional password players must enter before joining.", Type: model.FieldTypeSecret, Sensitive: true, Constraints: constraints(model.Constraints{MaxLength: integer(maximumGamePasswordLength)})},
				{Path: "rconPassword", Label: "RCON password", Description: "Platform-generated credential for the managed interactive console.", Type: model.FieldTypeSecret, Required: true, Sensitive: true, Generated: true, Constraints: constraints(model.Constraints{MinLength: integer(minimumRCONPasswordLength), MaxLength: integer(maximumRCONPasswordLength)})},
			},
		},
		Capabilities: []model.Capability{
			{ID: "configuration", Released: false, Description: "Tailored server settings backed by the typed Factorio configuration."},
			{ID: "mods", Released: true, Description: "Provider-backed Factorio mod management."},
			{ID: "saves", Released: true, Description: "Validated save export and destructive hosted-save replacement."},
			{ID: "console", Released: false, Description: "Interactive Factorio administration over RCON."},
			{ID: "logs", Released: true, Description: "Read-only live container and Factorio log output."},
		},
		Runtime: model.RuntimePolicy{Channels: []model.RuntimeChannel{
			{ID: "rcon", Label: "Game console", Interaction: model.InteractionInteractive, Protocol: "rcon", Released: false},
			{ID: "container-stdout", Label: "Server output", Interaction: model.InteractionReadOnly, Protocol: "container-stdout", Released: true},
			{ID: "factorio-log", Label: "Factorio log", Interaction: model.InteractionReadOnly, Protocol: "file", Released: false},
		}},
		Shutdown: model.ShutdownPolicy{Strategy: "rcon-command", Command: "/quit", TimeoutSeconds: 90, ForceSupported: true},
		Saves: model.SavePolicy{
			ImportReleased: true, ExportReleased: true, RequiresStopped: true,
			DestructiveImport: true, LeavesServerStopped: true, ArchiveFormat: "zip",
			MediaTypes: []string{"application/zip"}, FileExtensions: []string{".zip"},
			RequiredEntries: []string{levelEntryName, levelInitEntryName}, MaximumArchiveBytes: MaximumSaveArchiveBytes,
			MaximumExpandedBytes: MaximumSaveExpandedBytes, MaximumEntries: MaximumSaveEntries,
		},
		Mods: model.ModProviderPolicy{
			ProviderID: "factorio-mod-portal", ProviderName: "Factorio Mod Portal", ProviderURL: "https://mods.factorio.com",
			Released: true, NativeDiscovery: false, DirectReference: true, DependencyResolution: "provider-metadata",
			VersionSelection: "latest-compatible", Compatibility: "factorio-version", ApplyPolicy: "next-start",
			RequiresStopped: true, AutomaticRestart: false, ClientSynchronization: "join-time",
		},
	}
}
