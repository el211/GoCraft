package world

import "testing"

func TestCropsGrowWithoutLight(t *testing.T) {
	world := New(&FlatGenerator{}, nil, false)
	defer world.Close()
	world.SetBlock(0, 40, 0, Block{Namespace: "minecraft", Name: "farmland", Properties: map[string]string{"moisture": "0"}})
	world.SetBlock(0, 41, 0, Block{Namespace: "minecraft", Name: "wheat", Properties: map[string]string{"age": "0"}})
	world.SetBlock(0, 42, 0, Block{Namespace: "minecraft", Name: "stone"})

	for tick := int64(20); tick <= 400; tick += 20 {
		world.TickCrops(tick, 64)
		if world.GetBlock(0, 41, 0).Properties["age"] != "0" {
			return
		}
	}
	t.Fatal("covered wheat did not grow after 20 light-independent crop ticks")
}
