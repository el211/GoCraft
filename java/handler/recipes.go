package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"GoCraft/core/player"
	"GoCraft/internal/gamedata"
	"GoCraft/java/network"
	"GoCraft/java/protocol"
	javaworld "GoCraft/java/world"
)

const (
	recipeDisplayShapeless   int32 = 0
	recipeDisplayShaped      int32 = 1
	recipeDisplayFurnace     int32 = 2
	recipeDisplayStonecutter int32 = 3
	recipeDisplaySmithing    int32 = 4

	slotDisplayEmpty         int32 = 0
	slotDisplayAnyFuel       int32 = 1
	slotDisplayItem          int32 = 2
	slotDisplayStack         int32 = 3
	slotDisplayTag           int32 = 4
	slotDisplaySmithingTrim  int32 = 5
	slotDisplayWithRemainder int32 = 6
	slotDisplayComposite     int32 = 7
)

type recipeSlotDisplay struct {
	kind     int32
	item     string
	stack    player.ItemStack
	children []recipeSlotDisplay
}

type recipeDisplay struct {
	name        string
	kind        int32
	width       int32
	height      int32
	ingredients []recipeSlotDisplay
	result      recipeSlotDisplay
	station     string
	category    int32
	duration    int32
	experience  float32
	fuel        recipeSlotDisplay
}

type vanillaRecipeCatalog struct {
	Version string          `json:"_gocraft_version"`
	Recipes []vanillaRecipe `json:"recipes"`
}

type vanillaRecipe struct {
	Name        string                      `json:"name"`
	Type        string                      `json:"type"`
	Category    string                      `json:"category"`
	Group       string                      `json:"group"`
	Pattern     []string                    `json:"pattern"`
	Key         map[string]recipeIngredient `json:"key"`
	Ingredients []recipeIngredient          `json:"ingredients"`
	Ingredient  recipeIngredient            `json:"ingredient"`
	Input       recipeIngredient            `json:"input"`
	Material    recipeIngredient            `json:"material"`
	Template    recipeIngredient            `json:"template"`
	Base        recipeIngredient            `json:"base"`
	Addition    recipeIngredient            `json:"addition"`
	Result      recipeResult                `json:"result"`
	CookingTime int32                       `json:"cookingtime"`
	Experience  float32                     `json:"experience"`
}

type recipeIngredient struct {
	Values []string
}

func (ingredient *recipeIngredient) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		return nil
	}
	if data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		ingredient.Values = []string{value}
		return nil
	}
	if data[0] == '[' {
		return json.Unmarshal(data, &ingredient.Values)
	}
	return fmt.Errorf("recipe ingredient must be a resource location or array: %s", data)
}

type recipeResult struct {
	ID    string
	Count int
}

func (result *recipeResult) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] == '"' {
		if err := json.Unmarshal(data, &result.ID); err != nil {
			return err
		}
		result.Count = 1
		return nil
	}
	var object struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	result.ID, result.Count = object.ID, object.Count
	if result.Count == 0 && result.ID != "" {
		result.Count = 1
	}
	return nil
}

var (
	javaRecipeDisplays       []recipeDisplay
	javaRecipeLoadErr        error
	javaRecipeSourceCount    int
	javaRecipeDynamicRecipes []string
	javaItemTags             map[string]map[string]struct{}
)

func init() {
	javaRecipeDisplays, javaRecipeDynamicRecipes, javaRecipeSourceCount, javaItemTags, javaRecipeLoadErr = loadJavaRecipeDisplays()
}

func loadJavaRecipeDisplays() ([]recipeDisplay, []string, int, map[string]map[string]struct{}, error) {
	data, err := gamedata.FS.ReadFile("java/1.21.4/recipes.json")
	if err != nil {
		return nil, nil, 0, nil, fmt.Errorf("reading embedded recipes: %w", err)
	}
	var catalog vanillaRecipeCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, nil, 0, nil, fmt.Errorf("decoding embedded recipes: %w", err)
	}
	if catalog.Version != "1.21.4" {
		return nil, nil, len(catalog.Recipes), nil, fmt.Errorf("recipe catalog version %q, want 1.21.4", catalog.Version)
	}
	tags, err := loadJavaItemTags()
	if err != nil {
		return nil, nil, len(catalog.Recipes), nil, err
	}
	displays := make([]recipeDisplay, 0, len(catalog.Recipes))
	dynamic := make([]string, 0, 16)
	seen := make(map[string]struct{}, len(catalog.Recipes))
	for _, source := range catalog.Recipes {
		if _, exists := seen[source.Name]; exists {
			return nil, nil, len(catalog.Recipes), nil, fmt.Errorf("duplicate recipe %s", source.Name)
		}
		seen[source.Name] = struct{}{}
		display, fixed, err := convertVanillaRecipe(source)
		if err != nil {
			return nil, nil, len(catalog.Recipes), nil, fmt.Errorf("recipe %s: %w", source.Name, err)
		}
		if !fixed {
			dynamic = append(dynamic, source.Name)
			continue
		}
		displays = append(displays, display)
	}
	return displays, dynamic, len(catalog.Recipes), tags, nil
}

func loadJavaItemTags() (map[string]map[string]struct{}, error) {
	data, err := gamedata.FS.ReadFile("java/1.21.4/network_tags.json")
	if err != nil {
		return nil, fmt.Errorf("reading embedded item tags: %w", err)
	}
	var payload struct {
		Registries []struct {
			Name string `json:"name"`
			Tags []struct {
				Name    string  `json:"name"`
				Entries []int32 `json:"entries"`
			} `json:"tags"`
		} `json:"registries"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decoding embedded item tags: %w", err)
	}
	result := make(map[string]map[string]struct{})
	for _, registry := range payload.Registries {
		if registry.Name != "minecraft:item" {
			continue
		}
		for _, tag := range registry.Tags {
			items := make(map[string]struct{}, len(tag.Entries))
			for _, id := range tag.Entries {
				if name := javaworld.ItemName(id); name != "" {
					items[name] = struct{}{}
				}
			}
			result[tag.Name] = items
		}
		return result, nil
	}
	return nil, fmt.Errorf("embedded network tags do not contain minecraft:item")
}

func convertVanillaRecipe(source vanillaRecipe) (recipeDisplay, bool, error) {
	display := recipeDisplay{name: source.Name, fuel: recipeSlotDisplay{kind: slotDisplayAnyFuel}}
	resultSlot := stackRecipeSlot(source.Result)
	switch source.Type {
	case "minecraft:crafting_shaped":
		display.kind = recipeDisplayShaped
		display.station = "minecraft:crafting_table"
		display.category = craftingRecipeCategory(source.Category)
		display.height = int32(len(source.Pattern))
		if display.height == 0 {
			return recipeDisplay{}, false, fmt.Errorf("empty shaped pattern")
		}
		display.width = int32(utf8.RuneCountInString(source.Pattern[0]))
		for _, row := range source.Pattern {
			if int32(utf8.RuneCountInString(row)) != display.width {
				return recipeDisplay{}, false, fmt.Errorf("inconsistent shaped pattern width")
			}
			for _, symbol := range row {
				if symbol == ' ' {
					display.ingredients = append(display.ingredients, recipeSlotDisplay{kind: slotDisplayEmpty})
					continue
				}
				ingredient, ok := source.Key[string(symbol)]
				if !ok {
					return recipeDisplay{}, false, fmt.Errorf("pattern symbol %q has no key", symbol)
				}
				display.ingredients = append(display.ingredients, ingredientRecipeSlot(ingredient))
			}
		}
		display.result = resultSlot
	case "minecraft:crafting_shapeless":
		display.kind = recipeDisplayShapeless
		display.station = "minecraft:crafting_table"
		display.category = craftingRecipeCategory(source.Category)
		for _, ingredient := range source.Ingredients {
			display.ingredients = append(display.ingredients, ingredientRecipeSlot(ingredient))
		}
		display.result = resultSlot
	case "minecraft:crafting_transmute":
		display.kind = recipeDisplayShapeless
		display.station = "minecraft:crafting_table"
		display.category = craftingRecipeCategory(source.Category)
		display.ingredients = []recipeSlotDisplay{ingredientRecipeSlot(source.Input), ingredientRecipeSlot(source.Material)}
		display.result = resultSlot
	case "minecraft:smelting", "minecraft:blasting", "minecraft:smoking", "minecraft:campfire_cooking":
		display.kind = recipeDisplayFurnace
		display.ingredients = []recipeSlotDisplay{ingredientRecipeSlot(source.Ingredient)}
		display.result = resultSlot
		display.duration = source.CookingTime
		display.experience = source.Experience
		switch source.Type {
		case "minecraft:smelting":
			display.station = "minecraft:furnace"
			if source.Category == "food" {
				display.category = 4
			} else if source.Category == "blocks" {
				display.category = 5
			} else {
				display.category = 6
			}
		case "minecraft:blasting":
			display.station = "minecraft:blast_furnace"
			if source.Category == "blocks" {
				display.category = 7
			} else {
				display.category = 8
			}
		case "minecraft:smoking":
			display.station, display.category = "minecraft:smoker", 9
		case "minecraft:campfire_cooking":
			display.station, display.category = "minecraft:campfire", 12
			display.fuel = recipeSlotDisplay{kind: slotDisplayEmpty}
		}
	case "minecraft:stonecutting":
		display.kind = recipeDisplayStonecutter
		display.ingredients = []recipeSlotDisplay{ingredientRecipeSlot(source.Ingredient)}
		display.result = resultSlot
		display.station, display.category = "minecraft:stonecutter", 10
	case "minecraft:smithing_transform":
		display.kind = recipeDisplaySmithing
		display.ingredients = []recipeSlotDisplay{
			ingredientRecipeSlot(source.Template), ingredientRecipeSlot(source.Base), ingredientRecipeSlot(source.Addition),
		}
		display.result = resultSlot
		display.station, display.category = "minecraft:smithing_table", 11
	case "minecraft:smithing_trim":
		template := ingredientRecipeSlot(source.Template)
		base := ingredientRecipeSlot(source.Base)
		addition := ingredientRecipeSlot(source.Addition)
		display.kind = recipeDisplaySmithing
		display.ingredients = []recipeSlotDisplay{template, base, addition}
		display.result = recipeSlotDisplay{kind: slotDisplaySmithingTrim, children: []recipeSlotDisplay{base, addition, template}}
		display.station, display.category = "minecraft:smithing_table", 11
	default:
		if source.Type == "minecraft:crafting_decorated_pot" || strings.HasPrefix(source.Type, "minecraft:crafting_special_") {
			return recipeDisplay{}, false, nil
		}
		return recipeDisplay{}, false, fmt.Errorf("unsupported recipe type %s", source.Type)
	}
	return display, true, nil
}

func craftingRecipeCategory(category string) int32 {
	switch category {
	case "building":
		return 0
	case "redstone":
		return 1
	case "equipment":
		return 2
	default:
		return 3
	}
}

func ingredientRecipeSlot(ingredient recipeIngredient) recipeSlotDisplay {
	if len(ingredient.Values) == 0 {
		return recipeSlotDisplay{kind: slotDisplayEmpty}
	}
	children := make([]recipeSlotDisplay, 0, len(ingredient.Values))
	for _, value := range ingredient.Values {
		if strings.HasPrefix(value, "#") {
			children = append(children, recipeSlotDisplay{kind: slotDisplayTag, item: strings.TrimPrefix(value, "#")})
		} else {
			children = append(children, recipeSlotDisplay{kind: slotDisplayItem, item: value})
		}
	}
	if len(children) == 1 {
		return children[0]
	}
	return recipeSlotDisplay{kind: slotDisplayComposite, children: children}
}

func stackRecipeSlot(result recipeResult) recipeSlotDisplay {
	return recipeSlotDisplay{kind: slotDisplayStack, stack: player.ItemStack{ItemID: result.ID, Count: result.Count}}
}

func validateRecipeDisplays() error {
	if javaRecipeLoadErr != nil {
		return javaRecipeLoadErr
	}
	if javaRecipeSourceCount != len(javaRecipeDisplays)+len(javaRecipeDynamicRecipes) {
		return fmt.Errorf("recipe catalog accounting mismatch: source=%d fixed=%d dynamic=%d", javaRecipeSourceCount, len(javaRecipeDisplays), len(javaRecipeDynamicRecipes))
	}
	for index, recipe := range javaRecipeDisplays {
		if recipe.name == "" {
			return fmt.Errorf("recipe %d has no name", index)
		}
		for _, ingredient := range recipe.ingredients {
			if err := validateRecipeSlot(ingredient); err != nil {
				return fmt.Errorf("recipe %s ingredient: %w", recipe.name, err)
			}
		}
		if err := validateRecipeSlot(recipe.result); err != nil {
			return fmt.Errorf("recipe %s result: %w", recipe.name, err)
		}
		if javaworld.ItemID(recipe.station) < 0 {
			return fmt.Errorf("recipe %s has unknown station %s", recipe.name, recipe.station)
		}
		if recipe.kind == recipeDisplayShaped && int(recipe.width*recipe.height) != len(recipe.ingredients) {
			return fmt.Errorf("recipe %s shaped grid is %dx%d with %d slots", recipe.name, recipe.width, recipe.height, len(recipe.ingredients))
		}
	}
	return nil
}

func validateRecipeSlot(slot recipeSlotDisplay) error {
	switch slot.kind {
	case slotDisplayEmpty, slotDisplayAnyFuel:
		return nil
	case slotDisplayItem:
		if javaworld.ItemID(slot.item) < 0 {
			return fmt.Errorf("unknown item %s", slot.item)
		}
	case slotDisplayStack:
		if slot.stack.IsEmpty() || javaworld.ItemID(slot.stack.ItemID) < 0 {
			return fmt.Errorf("unknown or empty stack %s x%d", slot.stack.ItemID, slot.stack.Count)
		}
	case slotDisplayTag:
		if _, ok := javaItemTags[slot.item]; !ok {
			return fmt.Errorf("unknown item tag #%s", slot.item)
		}
	case slotDisplaySmithingTrim:
		if len(slot.children) != 3 {
			return fmt.Errorf("smithing trim has %d children", len(slot.children))
		}
		for _, child := range slot.children {
			if err := validateRecipeSlot(child); err != nil {
				return err
			}
		}
	case slotDisplayWithRemainder:
		if len(slot.children) != 2 {
			return fmt.Errorf("remainder display has %d children", len(slot.children))
		}
		for _, child := range slot.children {
			if err := validateRecipeSlot(child); err != nil {
				return err
			}
		}
	case slotDisplayComposite:
		if len(slot.children) == 0 {
			return fmt.Errorf("empty composite display")
		}
		for _, child := range slot.children {
			if err := validateRecipeSlot(child); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown slot display type %d", slot.kind)
	}
	return nil
}

func (slot recipeSlotDisplay) matches(item string) bool {
	switch slot.kind {
	case slotDisplayItem:
		return slot.item == item
	case slotDisplayStack:
		return slot.stack.ItemID == item
	case slotDisplayTag:
		_, ok := javaItemTags[slot.item][item]
		return ok
	case slotDisplayComposite:
		for _, child := range slot.children {
			if child.matches(item) {
				return true
			}
		}
	}
	return false
}

func sendRecipeBook(conn *network.ClientConn) error {
	if err := validateRecipeDisplays(); err != nil {
		return err
	}
	if err := conn.WritePacket(protocol.NewBuilder(packetIDUpdateRecipes).VarInt(0).VarInt(0).Build()); err != nil {
		return err
	}
	return conn.WritePacket(buildRecipeBookAdd())
}

func handlePlaceRecipeRequest(packet *protocol.Packet, p *player.Player, conn *network.ClientConn) error {
	reader := packet.Reader()
	windowID, err := protocol.ReadVarInt(reader)
	if err != nil {
		return fmt.Errorf("place recipe: reading container ID: %w", err)
	}
	recipeID, err := protocol.ReadVarInt(reader)
	if err != nil {
		return fmt.Errorf("place recipe: reading display ID: %w", err)
	}
	makeAll, err := protocol.ReadBool(reader)
	if err != nil {
		return fmt.Errorf("place recipe: reading make-all flag: %w", err)
	}
	if recipeID < 0 || int(recipeID) >= len(javaRecipeDisplays) {
		return fmt.Errorf("place recipe: display ID %d is outside the synchronized recipe set", recipeID)
	}
	return placeCraftingRecipe(p, conn, windowID, javaRecipeDisplays[recipeID], makeAll)
}

func buildRecipeBookAdd() *protocol.Packet {
	b := protocol.NewBuilder(packetIDRecipeBookAdd).VarInt(int32(len(javaRecipeDisplays)))
	for id, recipe := range javaRecipeDisplays {
		b.VarInt(int32(id))
		encodeRecipeDisplay(b, recipe)
		b.VarInt(0)
		b.VarInt(recipe.category)
		b.Bool(false)
		b.Byte(0x00)
	}
	b.Bool(true)
	return b.Build()
}

func encodeRecipeDisplay(b *protocol.Builder, recipe recipeDisplay) {
	b.VarInt(recipe.kind)
	switch recipe.kind {
	case recipeDisplayShapeless:
		b.VarInt(int32(len(recipe.ingredients)))
		for _, ingredient := range recipe.ingredients {
			encodeRecipeSlotDisplay(b, ingredient)
		}
		encodeRecipeSlotDisplay(b, recipe.result)
		encodeRecipeSlotDisplay(b, recipeSlotDisplay{kind: slotDisplayItem, item: recipe.station})
	case recipeDisplayShaped:
		b.VarInt(recipe.width).VarInt(recipe.height).VarInt(int32(len(recipe.ingredients)))
		for _, ingredient := range recipe.ingredients {
			encodeRecipeSlotDisplay(b, ingredient)
		}
		encodeRecipeSlotDisplay(b, recipe.result)
		encodeRecipeSlotDisplay(b, recipeSlotDisplay{kind: slotDisplayItem, item: recipe.station})
	case recipeDisplayFurnace:
		encodeRecipeSlotDisplay(b, recipe.ingredients[0])
		encodeRecipeSlotDisplay(b, recipe.fuel)
		encodeRecipeSlotDisplay(b, recipe.result)
		encodeRecipeSlotDisplay(b, recipeSlotDisplay{kind: slotDisplayItem, item: recipe.station})
		b.VarInt(recipe.duration).Float(recipe.experience)
	case recipeDisplayStonecutter:
		encodeRecipeSlotDisplay(b, recipe.ingredients[0])
		encodeRecipeSlotDisplay(b, recipe.result)
		encodeRecipeSlotDisplay(b, recipeSlotDisplay{kind: slotDisplayItem, item: recipe.station})
	case recipeDisplaySmithing:
		for _, ingredient := range recipe.ingredients {
			encodeRecipeSlotDisplay(b, ingredient)
		}
		encodeRecipeSlotDisplay(b, recipe.result)
		encodeRecipeSlotDisplay(b, recipeSlotDisplay{kind: slotDisplayItem, item: recipe.station})
	}
}

func encodeRecipeSlotDisplay(b *protocol.Builder, slot recipeSlotDisplay) {
	b.VarInt(slot.kind)
	switch slot.kind {
	case slotDisplayItem:
		b.VarInt(javaworld.ItemID(slot.item))
	case slotDisplayStack:
		encodeSlot(b, slot.stack)
	case slotDisplayTag:
		b.String(slot.item)
	case slotDisplaySmithingTrim, slotDisplayWithRemainder:
		for _, child := range slot.children {
			encodeRecipeSlotDisplay(b, child)
		}
	case slotDisplayComposite:
		b.VarInt(int32(len(slot.children)))
		for _, child := range slot.children {
			encodeRecipeSlotDisplay(b, child)
		}
	}
}
