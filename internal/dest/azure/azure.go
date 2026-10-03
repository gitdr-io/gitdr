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
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
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

	// maxBlocks is how many blocks a blob may be committed in. Zero is Azure's own limit,
	// blockblob.MaxBlocks; tests lower it, so a blob that needs larger blocks fits in megabytes.
	maxBlocks int
}

// accountName is Azure's rule for a storage account name: 3 to 24 lowercase letters and digits.
// The account becomes the first label of the blob endpoint's host.
var accountName = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

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
	// Refused before anything is built from it: the account is the host the credential is sent
	// to. Not quoted, because a value pasted into the wrong field can be a connection string.
	if opts.Account != "" && !accountName.MatchString(opts.Account) {
		return nil, errors.New("azure: the account is not a storage account name, which is 3 to 24 lowercase letters and digits")
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
		// construction; with either, the URL has to be that account's own endpoint in the cloud
		// Resource Manager is asked in, or a locked policy on some other container would be reported
		// as protecting this one. The URL is not printed: a SAS connection string puts the token in it.
		if !namesAccount(client.URL(), opts.Account) {
			return nil, fmt.Errorf("azure: the blob endpoint is not account %q's own in Azure's public cloud, where the immutability policy would be read", opts.Account)
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
	// How long the policy holds each blob, which a skip of an unchanged repository may not outlast.
	// Reported whatever the policy's state, because it can only shorten how long a copy is relied on.
	var held time.Duration
	if days != nil && *days > 0 {
		held = time.Duration(*days) * 24 * time.Hour
	}

	switch {
	case state != nil && *state == armstorage.ImmutabilityPolicyStateLocked && days != nil && *days > 0:
		return dest.WormStatus{
			Verdict: dest.VerdictImmutable,
			Period:  held,
			Mode:    "IMMUTABILITY",
			Details: fmt.Sprintf("container immutability policy Locked, %d days%s", *days, scope),
		}
	case state != nil && *state == armstorage.ImmutabilityPolicyStateUnlocked:
		// An earned negative, and the distinction is the reason this check exists. An unlocked
		// policy blocks deletes today and can be shortened or removed tomorrow by the person
		// most likely to be compromised, so nothing here is enforced against them.
		return dest.WormStatus{
			Verdict: dest.VerdictNotImmutable,
			Period:  held,
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
			Period:  held,
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

// dnsZone is the zone label of an Azure DNS zone endpoint, <account>.z<N>.blob.storage.azure.net,
// where Microsoft documents N from 1 to 50.
var dnsZone = regexp.MustCompile(`^z([1-9]|[1-4][0-9]|50)$`)

// namesAccount reports whether a blob service URL is the storage account's own endpoint in Azure's
// public cloud, which is the cloud Resource Manager is read in, with no container in its path:
//
//	https://<account>.blob.core.windows.net/
//	https://<account>.privatelink.blob.core.windows.net/
//	https://<account>.z<N>.blob.storage.azure.net/   an Azure DNS zone endpoint
//	http://127.0.0.1:10000/<account>                  an emulator on this machine
//
// Up to v0.1.20 any host whose first label was the account passed, and any URL whose first path
// segment was. Anyone can name a host acme.example.org, so a config could send the writes there while
// the lock was read off acme's container. A sovereign cloud's endpoint is another account however it
// is named, and a path past the account is another container.
func namesAccount(serviceURL, account string) bool {
	u, err := url.Parse(serviceURL)
	if err != nil || !accountName.MatchString(account) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	path := strings.TrimSuffix(u.Path, "/")
	if rest, ok := strings.CutPrefix(host, account+"."); ok && path == "" {
		switch rest {
		case "blob.core.windows.net", "privatelink.blob.core.windows.net":
			return true
		}
		zone, ok := strings.CutSuffix(rest, ".blob.storage.azure.net")
		return ok && dnsZone.MatchString(zone)
	}
	return isLoopback(host) && path == "/"+account
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PutImmutable uploads key create-only (If-None-Match: *); immutability is enforced by
// the container's immutability policy. Never overwrites, never deletes.
//
// The blocks are sized from size, so that no blob needs more of them than Azure commits. Up to
// v0.1.20 every blob went in the SDK's 1 MiB blocks, and one over 48.8 GiB, 50,000 of them, failed
// at Put Block List, after all of it had been sent.
//
// The size recorded is what was read from r and committed, and it has to be size: a stream that ran
// short or long is not the artifact that was checksummed. Up to v0.1.20 it recorded 0 for every blob.
func (b *Backend) PutImmutable(ctx context.Context, key string, r io.Reader, size int64, _ dest.Retention) (dest.PutResult, error) {
	maxBlocks := b.maxBlocks
	if maxBlocks <= 0 {
		maxBlocks = blockblob.MaxBlocks
	}
	blockSize, err := blockSizeFor(size, maxBlocks)
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("azure: put %q: %w", key, err)
	}
	etagAny := azcore.ETagAny
	sent := &countingReader{r: r}
	_, err = b.client.UploadStream(ctx, b.container, key, sent, &azblob.UploadStreamOptions{
		BlockSize: blockSize,
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: &etagAny},
		},
	})
	if err != nil {
		return dest.PutResult{}, fmt.Errorf("azure: put %q: %w", key, err)
	}
	if sent.n != size {
		return dest.PutResult{}, fmt.Errorf("azure: put %q: %d bytes were stored, and the artifact is %d", key, sent.n, size)
	}
	return dest.PutResult{Key: key, Size: sent.n}, nil
}

// blockSizeFor is the block size a blob of size bytes is sent in: the smallest whole number of MiB,
// and 1 MiB at least, that commits it in at most maxBlocks blocks. That is the SDK's own 1 MiB up to
// 48.8 GiB, so a blob below it goes as it always has. The upload holds as many blocks in memory as
// it sends at once, which is why the block is no larger than it has to be.
//
// Azure takes no block over blockblob.MaxStageBlockBytes, 4000 MiB, so a blob larger than maxBlocks
// of those, about 190.7 TiB, cannot be a block blob and is refused before anything is sent.
func blockSizeFor(size int64, maxBlocks int) (int64, error) {
	const mib = 1 << 20
	if size < 0 {
		return 0, fmt.Errorf("a size of %d bytes is not a length", size)
	}
	if maxBlocks < 1 {
		return 0, fmt.Errorf("a blob of at most %d blocks holds nothing", maxBlocks)
	}
	perBlock := size / int64(maxBlocks)
	if size%int64(maxBlocks) != 0 {
		perBlock++
	}
	if perBlock > blockblob.MaxStageBlockBytes {
		return 0, fmt.Errorf("%d bytes do not fit in a block blob of %d blocks of at most %d MiB", size, maxBlocks, blockblob.MaxStageBlockBytes/mib)
	}
	return max(1, (perBlock+mib-1)/mib) * mib, nil
}

// countingReader counts what is read through it. The upload reads its source from one goroutine.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
			if p := item.Properties; p != nil {
				if p.ContentLength != nil {
					o.Size = *p.ContentLength
				}
				switch {
				case p.CreationTime != nil:
					o.LastModified = *p.CreationTime
				case p.LastModified != nil:
					o.LastModified = *p.LastModified
				}
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
