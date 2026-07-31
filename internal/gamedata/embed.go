// Package gamedata embeds Minecraft's data-generator JSON reports and exposes
// them as an [embed.FS] so the Java (and future Bedrock) adapters can load
// registry data at startup without reading from the filesystem.
//
// The embedded files live under java/<version>/ and match the format produced
// by Minecraft's own data generator:
//
//	java -DbundlerMainClass=net.minecraft.data.Main -jar server.jar --reports
//
// To update coverage, replace the JSON files under java/1.21.4/ with the full
// generated output and rebuild.  No Go source changes are required.
package gamedata

import "embed"

// FS is a read-only view of all embedded game-data files.
//
//go:embed java/1.21.4/blocks.json java/1.21.4/items.json java/1.21.4/registries.json java/1.21.4/network_registries.json java/1.21.4/network_tags.json java/1.21.4/recipes.json
var FS embed.FS
