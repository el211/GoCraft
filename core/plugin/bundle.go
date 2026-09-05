// Package plugin owns plugin discovery, lifecycle, and event dispatch.
package plugin

import (
	abi "github.com/GoCraft-MC/gocraft-abi/abi/v1"
	"github.com/GoCraft-MC/gocraft-abi/gcpkg"
)

// Bundle is one archive this server found, and where it decided to keep that
// plugin's data.
//
// The archive half is the shared format: a build tool writes it and any host
// reads it. The data directory is not — it is this server's answer to where a
// plugin's files live, which no build tool has an opinion about and no other
// host has to agree with.
type Bundle struct {
	gcpkg.Bundle
	DataDirectory string

	// EventTypes binds the plugin-defined events this plugin provides or
	// subscribes to, to the ids this server assigned them.
	//
	// Here for the same reason DataDirectory is: the ids come from the full set
	// of manifests this particular server has installed, so they are a fact
	// about the server rather than about the archive. Another host with another
	// set of plugins numbers the same event differently, and neither is wrong.
	//
	// Filled by LoadAll from the registry built at preflight. A runtime reads
	// it and passes it on; it never assigns one.
	EventTypes []abi.EventBinding
}
