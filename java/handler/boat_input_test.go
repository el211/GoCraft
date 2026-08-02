package handler

import (
	"strings"
	"testing"

	coreplayer "GoCraft/core/player"
	"GoCraft/java/protocol"
)

func TestPlayerInputProtocol769UsesOneByteFlags(t *testing.T) {
	p := &coreplayer.Player{}
	pkt := &protocol.Packet{Data: []byte{0x01}}
	if err := HandlePlayerInputPacket(pkt, p, nil, nil, nil); err != nil {
		t.Fatalf("one-byte player input: %v", err)
	}
}

func TestPlayerInputRejectsMissingFlags(t *testing.T) {
	p := &coreplayer.Player{}
	err := HandlePlayerInputPacket(&protocol.Packet{}, p, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "flags") {
		t.Fatalf("missing flags error = %v", err)
	}
}
