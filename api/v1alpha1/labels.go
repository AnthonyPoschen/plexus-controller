package v1alpha1

// Standard labels that the controller applies to GameServer resources
// and all child objects it creates (Pods, Services, PVCs, ConfigMaps, etc.).
//
// These labels enable easy cross-resource lookup by customer, server, or game.
const (
	LabelServerID       = "plexus.gg/server-id"
	LabelGameServerUID  = "plexus.gg/gameserver-uid"
	LabelOwnerUserID    = "plexus.gg/owner-user-id"
	LabelGameID         = "plexus.gg/game-id"
	LabelSetupID        = "plexus.gg/setup-id"
	LabelComponent      = "app.kubernetes.io/component"
	ComponentGameServer = "game-server"
)
