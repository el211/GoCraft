package goplugin

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"GoCraft/runtime/link"
)

func (r *Runtime) newSupervisor(pluginID, executable string) *link.Supervisor {
	spawn := r.spawn(executable)
	directory := r.config.SocketDirectory
	if directory == "" {
		directory = os.TempDir()
	}
	return link.NewSupervisor(link.Config{
		Runtime:      runtimeLabel(pluginID),
		Directory:    directory,
		ABI:          abiVersion,
		TickRate:     r.config.TickRate,
		EventBudget:  r.config.EventBudget,
		StartTimeout: r.config.StartTimeout,
		Spawn:        spawn,
		OnEmit:       r.config.OnEmit,
	}, r.config.Liveness)
}

func (r *Runtime) spawn(executable string) link.Spawn {
	if r.config.Spawn != nil {
		return r.config.Spawn(executable)
	}
	return func(socket string) *exec.Cmd {
		command := exec.Command(executable,
			"--sock", socket,
			"--abi", strconv.Itoa(abiVersion),
		)
		command.Stdout = r.config.Stdout
		if command.Stdout == nil {
			command.Stdout = os.Stdout
		}
		command.Stderr = r.config.Stderr
		if command.Stderr == nil {
			command.Stderr = os.Stderr
		}
		return command
	}
}

func runtimeLabel(pluginID string) string {
	hash := sha256.Sum256([]byte(pluginID))
	return fmt.Sprintf("go-%x", hash[:8])
}
