package bedrock

import "testing"

func TestCreativeCatalogueIsPopulated(t *testing.T) {
	l := &Listener{}
	l.initCreativeContent()
	if len(l.creativeGroups) == 0 || len(l.creativeItems) < 1000 {
		t.Fatalf("creative catalogue = %d groups/%d items", len(l.creativeGroups), len(l.creativeItems))
	}
	for _, item := range l.creativeNames {
		if item.name == "minecraft:oak_log" {
			return
		}
	}
	t.Fatal("creative catalogue does not contain minecraft:oak_log")
}
