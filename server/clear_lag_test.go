package server

import (
	"testing"

	"GoCraft/config"
	corentity "GoCraft/core/entity"
)

func TestClearLagTargetSelection(t *testing.T) {
	targets := config.ClearLagTargets{DroppedItems: true, Projectiles: true, PrimedTNT: true}
	if !clearLagRemoves(corentity.TypeItem, targets) ||
		!clearLagRemoves(corentity.TypeArrow, targets) ||
		!clearLagRemoves(corentity.TypePrimedTNT, targets) {
		t.Fatal("configured transient entities were not selected")
	}
	if clearLagRemoves(corentity.TypeCow, targets) || clearLagRemoves(corentity.TypeZombie, targets) {
		t.Fatal("mobs were selected without their explicit options")
	}
}
