package handler

import (
	"path/filepath"
	"testing"
)

func TestWhitelistPersistsAndMatchesBothEditionsByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whitelist.json")
	if err := ConfigureWhitelist(path, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := AddWhitelistedPlayer("CrossPlayer"); err != nil {
		t.Fatal(err)
	}
	if err := SetWhitelistEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !IsWhitelisted("crossplayer") || IsWhitelisted("stranger") {
		t.Fatal("enabled whitelist did not use a case-insensitive shared name list")
	}
	if err := ConfigureWhitelist(path, false, nil); err != nil {
		t.Fatal(err)
	}
	if !WhitelistEnabled() || !IsWhitelisted("CROSSPLAYER") {
		t.Fatal("whitelist state did not survive reload")
	}
}
