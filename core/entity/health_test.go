package entity

import "testing"

func TestVanillaMobHealthValues(t *testing.T) {
	tests := map[EntityType]float32{
		TypeChicken: 4, TypeAllay: 20, TypeArmadillo: 12,
		TypeZombie: 20, TypeCreeper: 20, TypeSpider: 16,
		TypeEnderman: 40, TypeBogged: 16, TypeWitch: 26,
		TypeIronGolem: 100, TypeWarden: 500, TypeWither: 300,
	}
	for entityType, want := range tests {
		entity := New(1, [16]byte{}, entityType, 0, 0, 0)
		if entity.Health != want || entity.MaxHealth != want {
			t.Errorf("%s health = %v/%v, want %v", entityType, entity.Health, entity.MaxHealth, want)
		}
	}
}
