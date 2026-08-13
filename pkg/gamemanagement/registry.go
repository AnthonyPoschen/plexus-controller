// Package gamemanagement exposes controller-owned game contracts to consumers
// without requiring access to a running controller.
package gamemanagement

import (
	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"
)

func Schema(gameID string) (model.ManagementSchema, bool) {
	switch gameID {
	case factorio.GameID:
		return factorio.Schema(), true
	default:
		return model.ManagementSchema{}, false
	}
}
