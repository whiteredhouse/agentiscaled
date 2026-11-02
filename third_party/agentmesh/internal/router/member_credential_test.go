package router

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eclipse-biscuit/biscuit-go/v2/pb"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/agentmesh/api"
	"github.com/google/agentmesh/internal/identity"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
)

// A normal serialized credential whose decoded authority block has spare
// capacity exercises biscuit-go's append during Authorizer construction. The
// padding only varies an ordinary subject fact; it changes no authorization.
func concurrentMemberToken(t *testing.T, private ed25519.PrivateKey, member peer.ID) []byte {
	t.Helper()
	for padding := 1; padding <= 64; padding++ {
		token, _, err := identity.MintBiscuitToken(private, jwt.MapClaims{"sub": strings.Repeat("m", padding)}, nil,
			member, time.Now().Add(time.Hour), []string{"member"},
			[]*api.PolicyRole{{Name: "member", AllowedPublish: []string{"mcp://allowed"}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var envelope pb.Biscuit
		if err := proto.Unmarshal(token, &envelope); err != nil {
			t.Fatal(err)
		}
		block := envelope.Authority.Block
		if cap(block)-len(block) >= 4 {
			return token
		}
	}
	t.Fatal("could not construct the shared-append regression credential")
	return nil
}

func TestConcurrentMemberPublishAuthorization(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	member := newTestPeerID(t)
	token := concurrentMemberToken(t, private, member)
	admission, key, _, err := identity.VerifyBiscuitAdmission(token, member, []ed25519.PublicKey{public}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	router := &Router{config: Options{BiscuitTimeout: time.Second}}
	router.credentials.Store(member, newMemberCredential(token, key))
	rules, _ := api.BuildPolicyRules([]*api.PolicyRole{{Name: "member", AllowedPublish: []string{"inference://policy"}}}, nil)
	texts := api.PolicyRuleTexts(rules)
	parsed, err := api.ParseDatalogRules(texts)
	if err != nil {
		t.Fatal(err)
	}
	router.policy.Store(&meshPolicy{rules: parsed, texts: texts})
	const workers = 16
	const rounds = 8
	start := make(chan struct{})
	errors := make(chan error, workers+1)
	var done sync.WaitGroup
	done.Add(workers + 1)
	for range workers {
		go func() {
			defer done.Done()
			<-start
			for range rounds {
				if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "allowed"); err != nil {
					errors <- fmt.Errorf("explicit grant refused: %w", err)
					return
				}
				if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_INFERENCE, "policy"); err != nil {
					errors <- fmt.Errorf("current policy grant refused: %w", err)
					return
				}
				if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "denied"); err == nil {
					errors <- fmt.Errorf("ungranted service accepted")
					return
				}
			}
		}()
	}
	// The handshake checks the router role after storing the admitted
	// credential. It must not share mutable parsed state with directory calls.
	go func() {
		defer done.Done()
		<-start
		for range rounds {
			if err := identity.RequireRole(admission, key, "member", time.Second); err != nil {
				errors <- fmt.Errorf("admitted role refused: %w", err)
				return
			}
			if err := identity.RequireRole(admission, key, api.RoleRouter, time.Second); err == nil {
				errors <- fmt.Errorf("ungranted router role accepted")
				return
			}
		}
	}()
	close(start)
	done.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	// Policy remains current, rather than frozen when the credential is
	// admitted or first used. The explicit token grant remains independent.
	router.policy.Store(nil)
	if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_INFERENCE, "policy"); err == nil {
		t.Fatal("removed policy grant remained authorized")
	}
	if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "allowed"); err != nil {
		t.Fatalf("explicit grant changed with mesh policy: %v", err)
	}
}

func TestMemberCredentialOwnsAdmittedBytesAndKey(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	member := newTestPeerID(t)
	token := concurrentMemberToken(t, private, member)
	_, key, _, err := identity.VerifyBiscuitAdmission(token, member, []ed25519.PublicKey{public}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	router := &Router{config: Options{BiscuitTimeout: time.Second}}
	router.credentials.Store(member, newMemberCredential(token, key))
	// Both inputs may belong to a handshake buffer or a trusted-key snapshot
	// that the caller reuses after admission. The cache must own its copies.
	for i := range token {
		token[i] ^= 0xff
	}
	for i := range key {
		key[i] ^= 0xff
	}
	if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "allowed"); err != nil {
		t.Fatalf("cached credential changed with its source buffers: %v", err)
	}
	if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "denied"); err == nil {
		t.Fatal("ungranted service accepted")
	}
}

func TestMalformedCachedMemberCredentialFailsClosed(t *testing.T) {
	member := newTestPeerID(t)
	router := &Router{config: Options{BiscuitTimeout: time.Second}}
	router.credentials.Store(member, newMemberCredential([]byte("invalid credential"), make(ed25519.PublicKey, ed25519.PublicKeySize)))
	if err := router.requirePublish(member, api.ServiceType_SERVICE_TYPE_MCP, "allowed"); err == nil {
		t.Fatal("malformed admitted bytes granted publication")
	}
}
