package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"gitdr.io/gitdr/internal/dest"
)

// tokenAnswer is a credential that answers every token request the same way.
type tokenAnswer struct{ err error }

func (c tokenAnswer) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: "t"}, nil
}

// A token the chain cannot get is marked as no credential, whichever of its two errors says so, and
// keeps the SDK's error inside for the log. A token asked for after the deadline is the deadline's.
func TestATokenTheChainCannotGetIsMarkedNoCredentials(t *testing.T) {
	unavailable := azidentity.NewCredentialUnavailableError("DefaultAzureCredential: failed to acquire a token")
	refused := &azidentity.AuthenticationFailedError{}
	stopped, stop := context.WithCancel(context.Background())
	stop()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		marked error // nil for a token
	}{
		{"nothing in the chain to use", context.Background(), unavailable, dest.ErrNoCredentials},
		{"a credential the identity provider refused", context.Background(), refused, dest.ErrNoCredentials},
		{"a run stopped while the chain looked", stopped, unavailable, context.Canceled},
		{"a token", context.Background(), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := markedCredential{tokenAnswer{tc.err}}.GetToken(tc.ctx, policy.TokenRequestOptions{})
			if tc.marked == nil {
				if err != nil {
					t.Fatalf("a token came back as %v", err)
				}
				return
			}
			if !errors.Is(err, tc.marked) || !errors.Is(err, tc.err) {
				t.Errorf("GetToken = %v; want it marked %v with the SDK's error inside", err, tc.marked)
			}
			if tc.marked != dest.ErrNoCredentials && errors.Is(err, dest.ErrNoCredentials) {
				t.Errorf("a stopped run is marked as no credential: %v", err)
			}
		})
	}
}
