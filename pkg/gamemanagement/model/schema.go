// Package model defines the serialized game-management schema shared by game
// adapters and their consumers.
package model

type FieldType string

const (
	FieldTypeBoolean    FieldType = "boolean"
	FieldTypeInteger    FieldType = "integer"
	FieldTypeSecret     FieldType = "secret"
	FieldTypeString     FieldType = "string"
	FieldTypeStringList FieldType = "string-list"
)

type Interaction string

const (
	InteractionInteractive Interaction = "interactive"
	InteractionReadOnly    Interaction = "read-only"
)

type ManagementSchema struct {
	GameID        string              `json:"gameID"`
	Version       string              `json:"version"`
	DisplayName   string              `json:"displayName"`
	Configuration ConfigurationSchema `json:"configuration"`
	Secrets       SecretSchema        `json:"secrets"`
	Capabilities  []Capability        `json:"capabilities"`
	Runtime       RuntimePolicy       `json:"runtime"`
	Shutdown      ShutdownPolicy      `json:"shutdown"`
	Saves         SavePolicy          `json:"saves"`
	Mods          ModProviderPolicy   `json:"mods"`
	Broadcast     BroadcastPolicy     `json:"broadcast"`
}

type ConfigurationSchema struct {
	Sections []Section `json:"sections"`
}

type Section struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Fields      []Field `json:"fields"`
}

type Field struct {
	Path        string       `json:"path"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Type        FieldType    `json:"type"`
	Required    bool         `json:"required"`
	Sensitive   bool         `json:"sensitive"`
	Generated   bool         `json:"generated,omitempty"`
	Default     any          `json:"default,omitempty"`
	Constraints *Constraints `json:"constraints,omitempty"`
	Options     []Option     `json:"options,omitempty"`
}

type Constraints struct {
	Minimum   *int `json:"minimum,omitempty"`
	Maximum   *int `json:"maximum,omitempty"`
	MinLength *int `json:"minLength,omitempty"`
	MaxLength *int `json:"maxLength,omitempty"`
	MaxItems  *int `json:"maxItems,omitempty"`
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type SecretSchema struct {
	Version string  `json:"version"`
	Fields  []Field `json:"fields"`
}

type Capability struct {
	ID          string `json:"id"`
	Released    bool   `json:"released"`
	Description string `json:"description"`
}

type RuntimePolicy struct {
	Channels []RuntimeChannel `json:"channels"`
}

type RuntimeChannel struct {
	ID          string      `json:"id"`
	Label       string      `json:"label"`
	Interaction Interaction `json:"interaction"`
	Protocol    string      `json:"protocol"`
	Released    bool        `json:"released"`
}

type ShutdownPolicy struct {
	Strategy       string `json:"strategy"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	ForceSupported bool   `json:"forceSupported"`
}

type SavePolicy struct {
	ImportReleased       bool     `json:"importReleased"`
	ExportReleased       bool     `json:"exportReleased"`
	RequiresStopped      bool     `json:"requiresStopped"`
	DestructiveImport    bool     `json:"destructiveImport"`
	LeavesServerStopped  bool     `json:"leavesServerStopped"`
	ArchiveFormat        string   `json:"archiveFormat"`
	MediaTypes           []string `json:"mediaTypes"`
	FileExtensions       []string `json:"fileExtensions"`
	RequiredEntries      []string `json:"requiredEntries"`
	MaximumArchiveBytes  int64    `json:"maximumArchiveBytes"`
	MaximumExpandedBytes int64    `json:"maximumExpandedBytes"`
	MaximumEntries       int      `json:"maximumEntries"`
}

type ModProviderPolicy struct {
	ProviderID            string `json:"providerID"`
	ProviderName          string `json:"providerName"`
	ProviderURL           string `json:"providerURL"`
	Released              bool   `json:"released"`
	NativeDiscovery       bool   `json:"nativeDiscovery"`
	DirectReference       bool   `json:"directReference"`
	DependencyResolution  string `json:"dependencyResolution"`
	VersionSelection      string `json:"versionSelection"`
	Compatibility         string `json:"compatibility"`
	ApplyPolicy           string `json:"applyPolicy"`
	RequiresStopped       bool   `json:"requiresStopped"`
	AutomaticRestart      bool   `json:"automaticRestart"`
	ClientSynchronization string `json:"clientSynchronization"`
}

type BroadcastChannel string

const (
	BroadcastChannelChat    BroadcastChannel = "chat"
	BroadcastChannelConsole BroadcastChannel = "console"
)

// BroadcastPolicy is the adapter declaration for in-game maintenance notices.
// An unsupported game leaves Released false and omits a channel.
type BroadcastPolicy struct {
	Released            bool             `json:"released"`
	Channel             BroadcastChannel `json:"channel,omitempty"`
	Protocol            string           `json:"protocol,omitempty"`
	MaximumMessageRunes int              `json:"maximumMessageRunes,omitempty"`
	AutomaticRestart    bool             `json:"automaticRestart"`
}

func (policy BroadcastPolicy) Supported() bool {
	if policy.Released == false || policy.AutomaticRestart {
		return false
	}
	return policy.Channel == BroadcastChannelChat || policy.Channel == BroadcastChannelConsole
}
