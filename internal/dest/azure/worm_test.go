// The Azure WORM decision, tested through fakes of the two reads it makes.
//
// Azurite has no Resource Manager at all, and no immutability policies, so the only path an
// emulator can exercise is the plain container (see azure_test.go). Everything that decides
// whether a container is locked is therefore tested here, against fakes of the two narrow
// interfaces the backend reads through: the blob endpoint's container properties, and Resource
// Manager's Blob Containers - Get. The shapes are the SDK's own types, and the policy values
// follow Microsoft's example response for that call.
//
// What this does not prove: that a real storage account answers in those shapes. That needs a
// live account with a locked policy, which none of the test environments have.
package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	azcontainer "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/crypto"
	"gitdr.io/gitdr/internal/dest"
	"gitdr.io/gitdr/internal/pipeline"
	"gitdr.io/gitdr/internal/source"
)

// blobEndpoint fakes the data-plane container read.
type blobEndpoint struct {
	says azcontainer.GetPropertiesResponse
	err  error
}

func (e blobEndpoint) GetProperties(context.Context, *azcontainer.GetPropertiesOptions) (azcontainer.GetPropertiesResponse, error) {
	return e.says, e.err
}

// resourceManager fakes the one management-plane read, and records which container it was asked
// about, because a lock read off the wrong container is the worst answer this check can give.
type resourceManager struct {
	says  armstorage.BlobContainer
	err   error
	asked string
}

func (r *resourceManager) Get(_ context.Context, group, account, name string, _ *armstorage.BlobContainersClientGetOptions) (armstorage.BlobContainersClientGetResponse, error) {
	r.asked = group + "/" + account + "/" + name
	return armstorage.BlobContainersClientGetResponse{BlobContainer: r.says}, r.err
}

func ptr[T any](v T) *T { return &v }

var yes, no = ptr(true), ptr(false)

// endpointSays is what the blob endpoint reports about a container. Nil means the header was
// not sent, which is not the same as false.
func endpointSays(policy, versionLevel, legalHold *bool) blobEndpoint {
	return blobEndpoint{says: azcontainer.GetPropertiesResponse{
		HasImmutabilityPolicy:                   policy,
		IsImmutableStorageWithVersioningEnabled: versionLevel,
		HasLegalHold:                            legalHold,
	}}
}

// withPolicy is a container as Resource Manager reports it, holding a time-based retention
// policy in the given state.
func withPolicy(state armstorage.ImmutabilityPolicyState, days *int32, versionLevel bool) *resourceManager {
	return &resourceManager{says: armstorage.BlobContainer{ContainerProperties: &armstorage.ContainerProperties{
		HasImmutabilityPolicy: yes,
		ImmutabilityPolicy: &armstorage.ImmutabilityPolicyProperties{
			Properties: &armstorage.ImmutabilityPolicyProperty{State: ptr(state), ImmutabilityPeriodSinceCreationInDays: days},
		},
		ImmutableStorageWithVersioning: &armstorage.ImmutableStorageWithVersioning{Enabled: ptr(versionLevel)},
	}}}
}

// backend wires the fakes in. rm nil is a config without subscriptionID and resourceGroup.
func backend(e blobEndpoint, rm *resourceManager) *Backend {
	b := &Backend{container: "backups", account: "acct", resourceGroup: "rg", props: e, logger: slog.New(slog.DiscardHandler)}
	if rm != nil {
		b.resource = rm // assigned only when set: a typed nil in an interface is not nil
	}
	return b
}

// Immutable only on a policy Resource Manager reports as Locked, with its period.
//
// The first case is the bug this exists for. The container had version-level immutability
// enabled, the blob endpoint said so, and VerifyWorm answered immutable. That flag says blob
// versions can carry a policy, not that any does, and even a policy is only a lock once it is
// locked: an unlocked one can be shortened or deleted by the account owner. The manifest signed
// "immutable" for containers where nothing was.
func TestVerifyWormSaysImmutableOnlyOnALockedPolicy(t *testing.T) {
	locked, unlocked := armstorage.ImmutabilityPolicyStateLocked, armstorage.ImmutabilityPolicyStateUnlocked

	for _, tc := range []struct {
		name     string
		endpoint blobEndpoint
		rm       *resourceManager
		want     dest.WormVerdict
		details  []string
		wantErr  bool
	}{
		{
			name:     "version-level immutability alone, without Resource Manager",
			endpoint: endpointSays(no, yes, no),
			want:     dest.VerdictUnknown,
			details:  []string{"version-level immutability enabled", "locks nothing"},
		},
		{
			name:     "a policy whose lock the blob endpoint cannot see",
			endpoint: endpointSays(yes, no, no),
			want:     dest.VerdictUnknown,
			details:  []string{"has an immutability policy", "Resource Manager"},
		},
		{
			name:     "a plain container, an earned negative",
			endpoint: endpointSays(no, no, no),
			want:     dest.VerdictNotImmutable,
			details:  []string{"no immutability policy"},
		},
		{
			name:     "a legal hold is not a lock",
			endpoint: endpointSays(no, no, yes),
			want:     dest.VerdictNotImmutable,
			details:  []string{"legal hold", "cleared"},
		},
		{
			name:     "an endpoint that leaves the headers out",
			endpoint: endpointSays(nil, nil, nil),
			want:     dest.VerdictUnknown,
			details:  []string{"did not report whether it has an immutability policy"},
		},
		{
			name:     "an endpoint that answers one question of two",
			endpoint: endpointSays(no, nil, no),
			want:     dest.VerdictUnknown,
			details:  []string{"did not report whether it has version-level immutability"},
		},
		{
			name:     "a locked container policy",
			endpoint: endpointSays(yes, no, no),
			rm:       withPolicy(locked, ptr(int32(30)), false),
			want:     dest.VerdictImmutable,
			details:  []string{"Locked", "30 days"},
		},
		{
			name:     "a locked version-level default policy",
			endpoint: endpointSays(yes, yes, no),
			rm:       withPolicy(locked, ptr(int32(7)), true),
			want:     dest.VerdictImmutable,
			details:  []string{"Locked", "7 days", "version-level"},
		},
		{
			name:     "an unlocked policy",
			endpoint: endpointSays(yes, no, no),
			rm:       withPolicy(unlocked, ptr(int32(30)), false),
			want:     dest.VerdictNotImmutable,
			details:  []string{"Unlocked", "30 days", "shortened or deleted"},
		},
		{
			name:     "an unlocked version-level default policy",
			endpoint: endpointSays(yes, yes, no),
			rm:       withPolicy(unlocked, ptr(int32(1)), true),
			want:     dest.VerdictNotImmutable,
			details:  []string{"Unlocked", "version-level"},
		},
		{
			name:     "locked, with no period reported",
			endpoint: endpointSays(yes, no, no),
			rm:       withPolicy(locked, nil, false),
			want:     dest.VerdictUnknown,
			details:  []string{"Locked", "no retention period"},
		},
		{
			name:     "a state gitdr does not know",
			endpoint: endpointSays(yes, no, no),
			rm:       withPolicy("Frozen", ptr(int32(30)), false),
			want:     dest.VerdictUnknown,
			details:  []string{`"Frozen"`},
		},
		{
			name:     "version-level immutability and no container policy",
			endpoint: endpointSays(no, yes, no),
			rm: &resourceManager{says: armstorage.BlobContainer{ContainerProperties: &armstorage.ContainerProperties{
				HasImmutabilityPolicy:          no,
				ImmutableStorageWithVersioning: &armstorage.ImmutableStorageWithVersioning{Enabled: yes},
			}}},
			want:    dest.VerdictUnknown,
			details: []string{"account"},
		},
		{
			name:     "Resource Manager reports nothing to lock",
			endpoint: endpointSays(no, no, no),
			rm: &resourceManager{says: armstorage.BlobContainer{ContainerProperties: &armstorage.ContainerProperties{
				HasImmutabilityPolicy: no,
				LegalHold:             &armstorage.LegalHoldProperties{HasLegalHold: yes},
			}}},
			want:    dest.VerdictNotImmutable,
			details: []string{"no immutability policy", "legal hold"},
		},
		{
			name:     "Resource Manager returns no properties",
			endpoint: endpointSays(yes, no, no),
			rm:       &resourceManager{},
			want:     dest.VerdictUnknown,
		},
		{
			// Omitted is not false, on this API as on the blob endpoint.
			name:     "Resource Manager leaves the policy question out",
			endpoint: endpointSays(no, no, no),
			rm:       &resourceManager{says: armstorage.BlobContainer{ContainerProperties: &armstorage.ContainerProperties{}}},
			want:     dest.VerdictUnknown,
			details:  []string{"did not say"},
		},
		{
			name:     "Resource Manager refuses",
			endpoint: endpointSays(yes, no, no),
			rm:       &resourceManager{err: &azcore.ResponseError{ErrorCode: "AuthorizationFailed", StatusCode: 403}},
			want:     dest.VerdictUnknown,
			details:  []string{"AuthorizationFailed"},
		},
		{
			name:     "Resource Manager refuses without a code",
			endpoint: endpointSays(yes, no, no),
			rm:       &resourceManager{err: &azcore.ResponseError{StatusCode: 404}},
			want:     dest.VerdictUnknown,
			details:  []string{"HTTP 404"},
		},
		{
			// No token, no network: the pipeline records unknown and logs the error, the same
			// way it does for S3.
			name:     "no answer from Resource Manager",
			endpoint: endpointSays(yes, no, no),
			rm:       &resourceManager{err: errors.New("DefaultAzureCredential: no credential")},
			wantErr:  true,
		},
		{
			name:     "the blob endpoint fails",
			endpoint: blobEndpoint{err: errors.New("dial tcp: connection refused")},
			rm:       withPolicy(locked, ptr(int32(30)), false),
			wantErr:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := backend(tc.endpoint, tc.rm).VerifyWorm(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("no error, verdict %q (%s)", st.Verdict.Wire(), st.Details)
				}
				if st.Verdict.Immutable() {
					t.Fatalf("a failed read claimed immutable: %s", st.Details)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyWorm: %v", err)
			}
			if st.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q (%s)", st.Verdict.Wire(), tc.want.Wire(), st.Details)
			}
			for _, d := range tc.details {
				if !strings.Contains(st.Details, d) {
					t.Errorf("details %q do not say %q", st.Details, d)
				}
			}
			if tc.rm != nil && tc.rm.asked != "rg/acct/backups" {
				t.Errorf("Resource Manager was asked about %q, want the container being written to (rg/acct/backups)", tc.rm.asked)
			}
		})
	}
}

// A blob with no policy on it is an earned negative only in a container where policies live on
// blobs.
//
// Once a locked container-level policy reads as immutable, the retention check runs on the first
// object of every backup. Under a container-level policy no blob ever carries
// x-ms-immutability-policy-until-date, so reading its absence as "nothing holds this object"
// would downgrade every correctly locked container to not-immutable, and fail every run under
// --require-worm.
func TestObserveRetentionCountsAMissingPolicyOnlyWherePoliciesLiveOnBlobs(t *testing.T) {
	until := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		blob      blob.GetPropertiesResponse
		blobErr   error
		container blobEndpoint
		want      dest.RetentionObservation
		wantErr   bool
	}{
		{
			name:      "the blob carries a policy",
			blob:      blob.GetPropertiesResponse{ImmutabilityPolicyExpiresOn: &until},
			container: endpointSays(yes, yes, no),
			want:      dest.RetentionPresent,
		},
		{
			name:      "a version-level container, and the blob carries nothing",
			container: endpointSays(yes, yes, no),
			want:      dest.RetentionAbsent,
		},
		{
			name:      "a container-level policy, which blobs never show",
			container: endpointSays(yes, no, no),
			want:      dest.RetentionNotChecked,
		},
		{
			name:      "a container that does not say",
			container: endpointSays(nil, nil, nil),
			want:      dest.RetentionNotChecked,
		},
		{
			name:      "the blob read fails",
			blobErr:   errors.New("403"),
			container: endpointSays(yes, yes, no),
			want:      dest.RetentionNotChecked,
			wantErr:   true,
		},
		{
			name:      "the container read fails",
			container: blobEndpoint{err: errors.New("403")},
			want:      dest.RetentionNotChecked,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Backend{props: tc.container, blobs: func(context.Context, string) (blob.GetPropertiesResponse, error) {
				return tc.blob, tc.blobErr
			}}
			got, when, err := b.ObserveRetention(context.Background(), "github.com/octo/hello/2026-09-25/hello.bundle")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("observation = %q, want %q", got, tc.want)
			}
			if got == dest.RetentionPresent && !when.Equal(until) {
				t.Errorf("until = %s, want %s", when, until)
			}
		})
	}
}

// The policy read and the writes must be about the same container, and New refuses a config
// where they could not be.
func TestNewBindsThePolicyReadToTheContainerItWritesTo(t *testing.T) {
	emulator := fmt.Sprintf(devConnectionString, "http://127.0.0.1:10000")
	for _, tc := range []struct {
		name string
		opts Options
		ok   bool
	}{
		{name: "a subscription without a resource group", opts: Options{Container: "c", Account: "acct", SubscriptionID: "sub"}},
		{name: "a resource group without a subscription", opts: Options{Container: "c", Account: "acct", ResourceGroup: "rg"}},
		{name: "no account to ask about", opts: Options{Container: "c", Endpoint: "https://acct.blob.core.windows.net/", SubscriptionID: "sub", ResourceGroup: "rg"}},
		{name: "an endpoint for another account", opts: Options{Container: "c", Account: "acct", Endpoint: "https://other.blob.core.windows.net/", SubscriptionID: "sub", ResourceGroup: "rg"}},
		{name: "an emulator for another account", opts: Options{Container: "c", Account: "acct", ConnectionString: emulator, SubscriptionID: "sub", ResourceGroup: "rg"}},
		{name: "the account's own endpoint", opts: Options{Container: "c", Account: "acct", SubscriptionID: "sub", ResourceGroup: "rg"}, ok: true},
		{name: "a private endpoint for the account", opts: Options{Container: "c", Account: "acct", Endpoint: "https://acct.privatelink.blob.core.windows.net/", SubscriptionID: "sub", ResourceGroup: "rg"}, ok: true},
		{name: "an emulator for the account", opts: Options{Container: "c", Account: "devstoreaccount1", ConnectionString: emulator, SubscriptionID: "sub", ResourceGroup: "rg"}, ok: true},
		{name: "no Resource Manager at all", opts: Options{Container: "c", Account: "acct"}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := New(context.Background(), tc.opts, nil)
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted a policy read that is not bound to the container being written")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if wantRead := tc.opts.SubscriptionID != ""; (b.resource != nil) != wantRead {
				t.Errorf("reads the policy through Resource Manager: %v, want %v", b.resource != nil, wantRead)
			}
		})
	}
}

// The refusal above must not quote the endpoint: a SAS connection string carries its token there.
func TestNewDoesNotQuoteASASEndpoint(t *testing.T) {
	cs := "BlobEndpoint=https://other.blob.core.windows.net/;SharedAccessSignature=sv=2024-11-04&ss=b&srt=co&sp=rl&sig=THISISTHESECRETSIGNATURE"
	_, err := New(context.Background(), Options{Container: "c", Account: "acct", ConnectionString: cs, SubscriptionID: "sub", ResourceGroup: "rg"}, nil)
	if err == nil {
		t.Fatal("accepted an endpoint for another account")
	}
	if strings.Contains(err.Error(), "THISISTHESECRETSIGNATURE") {
		t.Fatalf("the error carries the SAS token: %v", err)
	}
}

var errStopped = errors.New("stopped after the WORM gate")

// stopAtEnumeration ends a backup at the first step after the WORM gate. A run that returns this
// error is one the gate let through, and nothing was written, because nothing gets written
// before the repositories are known.
type stopAtEnumeration struct{}

func (stopAtEnumeration) ListRepos(context.Context, source.Filter) ([]source.Repo, error) {
	return nil, errStopped
}
func (stopAtEnumeration) CloneURL(context.Context, source.Repo) (string, error) {
	return "", errStopped
}
func (stopAtEnumeration) FetchMetadata(context.Context, source.Repo) ([]byte, error) {
	return nil, errStopped
}

// The flag's contract on Azure, through the pipeline's own gate: warn and proceed by default,
// fail closed under --require-worm, for the two kinds of container this check reclassified.
func TestRequireWormFailsClosedOnAnAzureContainerThatIsNotLocked(t *testing.T) {
	_, privPEM, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := crypto.ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	unlocked := func() *Backend {
		return backend(endpointSays(yes, no, no), withPolicy(armstorage.ImmutabilityPolicyStateUnlocked, ptr(int32(30)), false))
	}
	versioningOnly := func() *Backend { return backend(endpointSays(no, yes, no), nil) }
	locked := func() *Backend {
		return backend(endpointSays(yes, no, no), withPolicy(armstorage.ImmutabilityPolicyStateLocked, ptr(int32(30)), false))
	}

	for _, tc := range []struct {
		name     string
		b        *Backend
		require  bool
		proceeds bool
		logged   string
	}{
		{name: "unlocked, default", b: unlocked(), proceeds: true, logged: "NOT WORM-immutable"},
		{name: "unlocked, --require-worm", b: unlocked(), require: true},
		{name: "version-level immutability alone, default", b: versioningOnly(), proceeds: true, logged: "could not read this destination's immutability"},
		{name: "version-level immutability alone, --require-worm", b: versioningOnly(), require: true},
		{name: "locked, --require-worm", b: locked(), require: true, proceeds: true, logged: "destination is WORM-immutable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			_, err := pipeline.Backup(context.Background(), pipeline.BackupDeps{
				Config: config.Default(), Source: stopAtEnumeration{}, Dest: tc.b,
				SigningKey: signer, ToolVersion: "test", RequireWORM: tc.require,
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
			})
			if err == nil {
				t.Fatal("the run ended without an error; the stub source should have stopped it")
			}
			if tc.proceeds {
				if !errors.Is(err, errStopped) {
					t.Fatalf("the WORM gate stopped the run: %v", err)
				}
				if !strings.Contains(logs.String(), tc.logged) {
					t.Errorf("the gate did not say %q:\n%s", tc.logged, logs.String())
				}
				return
			}
			if errors.Is(err, errStopped) {
				t.Fatal("--require-worm let a container that is not locked through the WORM gate")
			}
			if !strings.Contains(err.Error(), "worm.require") {
				t.Errorf("the refusal does not name the setting that caused it: %v", err)
			}
		})
	}
}
