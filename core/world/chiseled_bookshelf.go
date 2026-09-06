package world

import "fmt"

// chiseled_bookshelf.go: six-slot book storage for minecraft:chiseled_bookshelf.
//
// Slot layout (viewed from the front):
//   0 | 1 | 2   (top row)
//   3 | 4 | 5   (bottom row)
//
// Slot targeting uses the cursor click position on the face. The facing
// direction determines which axis maps to columns.

// IsChiseledBookshelf reports whether the block is a chiseled bookshelf.
func IsChiseledBookshelf(name string) bool {
	return name == "minecraft:chiseled_bookshelf"
}

// IsBookshelfBook reports whether an item can be stored in a chiseled bookshelf.
func IsBookshelfBook(itemID string) bool {
	switch itemID {
	case "minecraft:book", "minecraft:written_book",
		"minecraft:writable_book", "minecraft:enchanted_book":
		return true
	}
	return false
}

// bookshelfSlotProp returns the block-state property name for a slot index.
func bookshelfSlotProp(slot int) string {
	return fmt.Sprintf("slot_%d_occupied", slot)
}

// ChiseledBookshelfSlot determines which slot (0-5) was targeted by a click.
// facing is the block's facing direction ("north","south","east","west").
// cx, cy, cz are the cursor position on the clicked face (each in [0,1]).
func ChiseledBookshelfSlot(facing string, cx, cy, cz float64) int {
	var col int
	switch facing {
	case "south":
		// cx increases west→east from viewer looking north; col left→right = 0→2.
		if cx < 1.0/3.0 {
			col = 0
		} else if cx < 2.0/3.0 {
			col = 1
		} else {
			col = 2
		}
	case "north":
		// Mirrored vs south.
		if cx > 2.0/3.0 {
			col = 0
		} else if cx > 1.0/3.0 {
			col = 1
		} else {
			col = 2
		}
	case "east":
		// cz increases south→north; col left→right (looking west) = south→north.
		if cz > 2.0/3.0 {
			col = 0
		} else if cz > 1.0/3.0 {
			col = 1
		} else {
			col = 2
		}
	case "west":
		if cz < 1.0/3.0 {
			col = 0
		} else if cz < 2.0/3.0 {
			col = 1
		} else {
			col = 2
		}
	}
	var row int
	if cy < 0.5 {
		row = 1
	}
	return row*3 + col
}

// InsertBookshelfBook places a book in the specified slot. Returns the updated
// block and true on success (slot must be empty).
func InsertBookshelfBook(block Block, slot int, _ string) (Block, bool) {
	if !IsChiseledBookshelf(block.ResourceLocation()) || slot < 0 || slot > 5 {
		return block, false
	}
	prop := bookshelfSlotProp(slot)
	if block.Properties[prop] == "true" {
		return block, false // slot occupied
	}
	updated := copyWorldBlock(block)
	updated.Properties[prop] = "true"
	return updated, true
}

// EjectBookshelfBook removes the book from the specified slot. Returns the
// book item ID, the updated block, and true on success.
func EjectBookshelfBook(block Block, slot int, storedItem string) (itemID string, updated Block, ok bool) {
	if !IsChiseledBookshelf(block.ResourceLocation()) || slot < 0 || slot > 5 {
		return "", block, false
	}
	prop := bookshelfSlotProp(slot)
	if block.Properties[prop] != "true" || storedItem == "" {
		return "", block, false
	}
	cleared := copyWorldBlock(block)
	cleared.Properties[prop] = "false"
	return storedItem, cleared, true
}

// BookshelfOccupiedCount returns how many slots are occupied.
func BookshelfOccupiedCount(block Block) int {
	n := 0
	for i := 0; i < 6; i++ {
		if block.Properties[bookshelfSlotProp(i)] == "true" {
			n++
		}
	}
	return n
}
