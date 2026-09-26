// Package azure implements the create-only Destination for Azure Blob Storage. WORM is a
// time-based retention policy on the container that has been locked; writes are create-only
// via If-None-Match. Auth uses DefaultAzureCredential. There is no delete path.
package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	azcontainer "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"gitdr.io/gitdr/internal/dest"
)

// Options configures the Azure Blob backend. Credentials come from DefaultAzureCredential
// (real Azure) or a connection string (emulator / shared key).
type Options struct {
	Account          string // storage account (real Azure)
	Container        string
	Endpoint         string // override service URL; default https://<account>.blob.core.windows.net/
	ConnectionString string // for Azurite or shared-key auth

	// SubscriptionID and ResourceGroup locate the storage account in Azure Resource Manager.
	//
	// Optional, and without them the WORM check never says immutable. Whether a container's
	// immutability policy is locked is reported only by Resource Manager: the blob endpoint says
	// a policy exists and nothing about whether it can still be shortened or deleted. The read
	// uses DefaultAzureCredential even when a connection string is set, because an account key
	// does not authenticate to Resource Manager.
	SubscriptionID string
	ResourceGroup  string
}

// Backend is a create-only Azure Blob Destination.
type Backend struct {
	client    *azblob.Client
	container string
	logger    *slog.Logger

	// The reads the immutability checks make, each narrowed to the one call it needs. The SDK
	// clients behind them can delete a container, clear a legal hold and shorten an unlocked
	// policy. These fields can do none of that, and declaring them this way is what makes it so.
	props    containerProps
	blobs    func(ctx context.Context, key string) (blob.GetPropertiesResponse, error)
	resource containerResource // nil unless SubscriptionID and ResourceGroup are set

	account       string
	resourceGroup string
}

// containerProps is the data-plane read: what the blob endpoint says about the container.
type containerProps interface {
	GetProperties(ctx context.Context, o *azcontainer.GetPropertiesOptions) (azcontainer.GetPropertiesResponse, error)
}

// containerResource is the management-plane read: the container as Resource Manager sees it,
// which is the only answer that includes the state and period of its immutability policy.
type containerResource interface {
	Get(ctx context.Context, resourceGroupName, accountName, containerName string, options *armstorage.BlobContainersClientGetOptions) (armstorage.BlobContainersClientGetResponse, error)
}

var _ dest.Destination = (*Backend)(nil)

// New builds an Azure Blob backend.
func New(_ context.Context, opts Options, logger *slog.Logger) (*Backend, error) {
	if strings.TrimSpace(opts.Container) == "" {
		return nil, errors.New("azure: container is required")
	}
	readPolicy := opts.SubscriptionID != "" || opts.ResourceGroup != ""
	if readPolicy && (opts.SubscriptionID == "" || opts.ResourceGroup == "" || opts.Account == "") {
		return nil, errors.New("azure: reading the immutability policy through Resource Manager needs subscriptionID, resourceGroup and account together")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var cred azcore.TokenCredential
	if opts.ConnectionString == "" || readPolicy {
		c, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("azure: default credential: %w", err)
		}
		cred = c
	}
	var client *azblob.Client
	var err error
	if opts.ConnectionString != "" {
		client, err = azblob.NewClientFromConnectionString(opts.ConnectionString, nil)
	} else {
		serviceURL := opts.Endpoint
		if serviceURL == "" {
			if opts.Account == "" {
				return nil, errors.New("azure: account or endpoint is required")
			}
			serviceURL = fmt.Sprintf("https://%s.blob.core.windows.net/", opts.Account)
		}
		client, err = azblob.NewClient(serviceURL, cred, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("azure: new client: %w", err)
	}

	b := &Backend{
		client:        client,
		container:     opts.Container,
		logger:        logger,
		props:         client.ServiceClient().NewContainerClient(opts.Container),
		account:       opts.Account,
		resourceGroup: opts.ResourceGroup,
	}
	b.blobs = func(ctx context.Context, key string) (blob.GetPropertiesResponse, error) {
		return client.ServiceClient().NewContainerClient(opts.Container).NewBlobClient(key).GetProperties(ctx, nil)
	}
	if readPolicy {
		// The policy read and the writes must be about the same container. With neither an
		// endpoint nor a connection string the blob URL is built from Account, so they agree by
		// construction; with either, the URL has to name the account Resource Manager is asked
		// about, or a locked policy on some other account's container would be reported as
		// protecting this one. The URL is not printed: a SAS connection string puts the token in it.
		if !namesAccount(client.URL(), opts.Account) {
			return nil, fmt.Errorf("azure: the blob endpoint does not belong to account %q, which is where the immutability policy would be read", opts.Account)
		}
		rm, err := armstorage.NewBlobContainersClient(opts.SubscriptionID, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("azure: resource manager client: %w", err)
		}
		b.resource = rm
	}
	return b, nil
}

// VerifyWorm says immutable only for a container whose time-based retention policy Resource
// Manager reports as Locked.
//
// It used to say immutable whenever the container had version-level immutability enabled. That
// flag says blob versions can carry policies, not that any does: a container with the flag and no
// policy protects nothing, and an unlocked policy can be shortened or deleted by the account
// owner, the identity an attacker is most likely to hold. The blob endpoint cannot tell any of
// that from a locked policy. It reports that a policy exists (x-ms-has-immutability-policy) and
// never its state or its period, so without the Resource Manager read the honest answer is
// unknown.
//
//	immutable      Resource Manager reported the container's policy Locked, with its period
//	not-immutable  the store answered and nothing here is locked: no policy and no version-level
//	               immutability, or a policy that is Unlocked
//	unknown        a policy or version-level immutability exists and its lock was not read, or
//	               Resource Manager refused the question
func (b *Backend) VerifyWorm(ctx context.Context) (dest.WormStatus, error) {
	props, err := b.props.GetProperties(ctx, nil)
	if err != nil {
		return dest.WormStatus{}, fmt.Errorf("azure: container properties: %w", err)
	}
	if b.resource == nil {
		return verdictFromDataPlane(props), nil
	}
	got, err := b.resource.Get(ctx, b.resourceGroup, b.account, b.container, nil)
	if err != nil {
		// Resource Manager answered and would not say, which is a refusal and not a no. The code
		// is kept because AuthorizationFailed and ResourceGroupNotFound send an operator to two
		// different places. Anything without a response (no token, no network) is returned as an
		// error, the way the S3 backend returns one, and the pipeline records unknown.
		var re *azcore.ResponseError
		if errors.As(err, &re) {
			return dest.WormStatus{
				Verdict: dest.VerdictUnknown,
				Details: "could not read the container's immutability policy: Resource Manager answered " + responseCode(re),
			}, nil
		}
		return dest.WormStatus{}, fmt.Errorf("azure: read container immutability policy: %w", err)
	}
	return verdictFromResourceManager(got.BlobContainer), nil
}

// verdictFromDataPlane is everything the blob endpoint alone can support. It can earn a negative
// and never a positive: a container that reports no policy and no version-level immutability
// holds nothing, but one that reports either has a lock state the blob endpoint does not carry.
func verdictFromDataPlane(p azcontainer.GetPropertiesResponse) dest.WormStatus {
	switch {
	case isTrue(p.HasImmutabilityPolicy):
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Mode:    "IMMUTABILITY",
			Details: "container has an immutability policy; whether it is locked is reported only by Resource Manager, and subscriptionID and resourceGroup are not set",
		}
	case isTrue(p.IsImmutableStorageWithVersioningEnabled):
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Details: "version-level immutability enabled, no container policy reported; that alone locks nothing, and a policy set on the account is not read",
		}
	case isFalse(p.HasImmutabilityPolicy) && isFalse(p.IsImmutableStorageWithVersioningEnabled):
		// The store answered both questions, so this negative is earned.
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Details: withLegalHold("no immutability policy on container, no version-level immutability", p.HasLegalHold),
		}
	default:
		// A header the store did not send is not a false. Azurite leaves some of them out.
		missing := "an immutability policy"
		if p.HasImmutabilityPolicy != nil {
			missing = "version-level immutability"
		}
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Details: "the container did not report whether it has " + missing,
		}
	}
}

// verdictFromResourceManager decides from the container as Resource Manager reports it. This is
// the only path that can say immutable, and only on a Locked policy with a period.
func verdictFromResourceManager(c armstorage.BlobContainer) dest.WormStatus {
	p := c.ContainerProperties
	if p == nil {
		return dest.WormStatus{Verdict: dest.VerdictUnknown, Details: "Resource Manager returned no container properties"}
	}
	versionLevel := p.ImmutableStorageWithVersioning != nil && isTrue(p.ImmutableStorageWithVersioning.Enabled)
	var state *armstorage.ImmutabilityPolicyState
	var days *int32
	if p.ImmutabilityPolicy != nil && p.ImmutabilityPolicy.Properties != nil {
		state = p.ImmutabilityPolicy.Properties.State
		days = p.ImmutabilityPolicy.Properties.ImmutabilityPeriodSinceCreationInDays
	}
	scope := ""
	if versionLevel {
		scope = ", version-level"
	}

	switch {
	case state != nil && *state == armstorage.ImmutabilityPolicyStateLocked && days != nil && *days > 0:
		return dest.WormStatus{
			Verdict: dest.VerdictImmutable,
			Mode:    "IMMUTABILITY",
			Details: fmt.Sprintf("container immutability policy Locked, %d days%s", *days, scope),
		}
	case state != nil && *state == armstorage.ImmutabilityPolicyStateUnlocked:
		// An earned negative, and the distinction is the reason this check exists. An unlocked
		// policy blocks deletes today and can be shortened or removed tomorrow by the person
		// most likely to be compromised, so nothing here is enforced against them.
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Mode:    "IMMUTABILITY",
			Details: fmt.Sprintf("container immutability policy Unlocked%s%s; an unlocked policy can be shortened or deleted", period(days), scope),
		}
	case state != nil && *state == armstorage.ImmutabilityPolicyStateLocked:
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Mode:    "IMMUTABILITY",
			Details: "container immutability policy Locked, with no retention period reported",
		}
	case state != nil:
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Mode:    "IMMUTABILITY",
			Details: fmt.Sprintf("container immutability policy in state %q, which is not Locked or Unlocked", string(*state)),
		}
	case isTrue(p.HasImmutabilityPolicy):
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Mode:    "IMMUTABILITY",
			Details: "container has an immutability policy and Resource Manager reported no state for it",
		}
	case versionLevel:
		// Blobs here may inherit a policy set on the storage account. gitdr does not read account
		// policies, so this is not a no.
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Details: "version-level immutability enabled, no container policy; that alone locks nothing, and a policy set on the account is not read",
		}
	case isFalse(p.HasImmutabilityPolicy):
		// Resource Manager said there is no policy, and version-level immutability is off, so no
		// policy on the account can reach these blobs either. Earned.
		legalHold := p.HasLegalHold
		if legalHold == nil && p.LegalHold != nil {
			legalHold = p.LegalHold.HasLegalHold
		}
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Details: withLegalHold("no immutability policy on container, no version-level immutability", legalHold),
		}
	default:
		// A response that leaves the question out has not answered it.
		return dest.WormStatus{
			Verdict: dest.VerdictUnknown,
			Details: "Resource Manager did not say whether the container has an immutability policy",
		}
	}
}

// A legal hold blocks deletes until someone with the right role clears it, which makes it the
// same kind of protection as an unlocked policy. Said, because an operator who set one will want
// to know why it did not count.
func withLegalHold(details string, hold *bool) string {
	if isTrue(hold) {
		return details + "; a legal hold is set, which can be cleared"
	}
	return details
}

func period(days *int32) string {
	if days == nil || *days <= 0 {
		return ""
	}
	return fmt.Sprintf(", %d days", *days)
}

func responseCode(re *azcore.ResponseError) string {
	if re.ErrorCode != "" {
		return re.ErrorCode
	}
	return fmt.Sprintf("HTTP %d", re.StatusCode)
}

func isTrue(b *bool) bool  { return b != nil && *b }
func isFalse(b *bool) bool { return b != nil && !*b }

// namesAccount reports whether a blob service URL is the storage account's own endpoint: the
// account as the first label of the host (account.blob.core.windows.net, a private endpoint, a
// sovereign cloud) or as the first path segment (an emulator, 127.0.0.1:10000/devstoreaccount1).
func namesAccount(serviceURL, account string) bool {
	u, err := url.Parse(serviceURL)
	if err != nil || account == "" {
		return false
	}
	if label, _, ok := strings.Cut(u.Hostname(), "."); ok && strings.EqualFold(label, account) {
		return true
	}
	segment, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return strings.EqualFold(segment, account)
}

// PutImmutable uploads key create-only (If-None-Match: *); immutability is enforced by
// the container's immutability policy. Never overwrites, never deletes.
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, _ int64, _ dest.Retention) (dest.PutResult, error) {
	etagAny := azcore.ETagAny
	_, err := b.client.UploadStream(ctx, b.container, key, r, &azblob.UploadStreamOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &etagAny},
		},
	})
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("azure: put %q: %w", key, err)
	}
	return dest.PutResult{Key: key}, nil
}

// List returns blobs under prefix (read-only).
func (b *Backend) List(ctx context.Context, prefix string) ([]dest.Object, error) {
	var out []dest.Object
	pager := b.client.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: list %q: %w", prefix, err)
		}
		for _, item := range page.Segment.BlobItems {
			o := dest.Object{Key: *item.Name}
			if item.Properties != nil && item.Properties.ContentLength != nil {
				o.Size = *item.Properties.ContentLength
			}
			out = append(out, o)
		}
	}
	return out, nil
}

// Get opens key for reading (read-only). Caller closes the reader.
func (b *Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := b.client.DownloadStream(ctx, b.container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("azure: get %q: %w", key, err)
	}
	return resp.Body, nil
}

// ObserveRetention asks Blob Storage what immutability is actually on an object gitdr wrote.
//
// Azure's model is container-level rather than per-object: `PutImmutable` sends no retention and
// records none, so unlike S3 there is no per-object date in the manifest to be wrong. What can
// still be wrong is the same shape one level up: a container that reports a locked policy while
// blobs come back without one is the identical unearned claim.
//
// `x-ms-immutability-policy-until-date` is returned only when a policy is set on the blob, and a
// policy can be set on a blob only in a container with version-level immutability. There, a blob
// without one is the earned negative: the container's default was not applied to this write. In
// a container protected by a container-level policy the blob never carries the header, however
// locked the container is, so its absence says nothing and is reported as not checked. Reading
// that absence as "nothing holds this object" is exactly the false negative this type exists to
// prevent. A failed read says nothing either.
func (b *Backend) ObserveRetention(ctx context.Context, key string) (dest.RetentionObservation, time.Time, error) {
	props, err := b.blobs(ctx, key)
	if err != nil {
		return dest.RetentionNotChecked, time.Time{}, fmt.Errorf("azure: blob properties %q: %w", key, err)
	}
	if props.ImmutabilityPolicyExpiresOn != nil && !props.ImmutabilityPolicyExpiresOn.IsZero() {
		return dest.RetentionPresent, props.ImmutabilityPolicyExpiresOn.UTC(), nil
	}
	c, err := b.props.GetProperties(ctx, nil)
	if err != nil {
		return dest.RetentionNotChecked, time.Time{}, fmt.Errorf("azure: container properties: %w", err)
	}
	if isTrue(c.IsImmutableStorageWithVersioningEnabled) {
		return dest.RetentionAbsent, time.Time{}, nil
	}
	return dest.RetentionNotChecked, time.Time{}, nil
}
