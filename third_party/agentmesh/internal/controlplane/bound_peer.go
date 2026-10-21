// agentiscale fork patch (see AGENTISCALE.md): a bootstrap token can be bound
// to one peer, so that a node handing its token on cannot let another key in.

package controlplane

import (
	"errors"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
)

// boundPeerPrefix starts a bootstrap token's description when the token
// admits only one peer: "agentiscale-bound-peer:<peer ID>", optionally
// followed by a space and a free-form note. The binding is kept in the
// description so that the fork needs no schema or API change.
const boundPeerPrefix = "agentiscale-bound-peer:"

var errInvalidBoundPeer = errors.New("bootstrap token names an invalid bound peer")

// tokenBoundPeer returns the canonical peer ID a token is bound to, or "" when
// it is not bound. A description that starts with the prefix but names no
// valid peer ID is an error, so such a token admits no one.
func tokenBoundPeer(description string) (string, error) {
	rest, ok := strings.CutPrefix(description, boundPeerPrefix)
	if !ok {
		return "", nil
	}
	id, _, _ := strings.Cut(rest, " ")
	pID, err := peer.Decode(id)
	if err != nil {
		return "", errInvalidBoundPeer
	}
	return pID.String(), nil
}
