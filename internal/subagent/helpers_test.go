package subagent

import (
	"github.com/rs/zerolog"
)

// Shared test literals (goconst).
const (
	tChainRef = "p::m1"
	tTask     = "work"
)

func testLogger() zerolog.Logger { return zerolog.Nop() }

func testOwner(sessionID, address string) Owner {
	return Owner{
		SessionID: sessionID,
		Address:   address,
		Kind:      OwnerChannel,
		Delivery:  Delivery{Kind: DeliveryPassive},
	}
}
