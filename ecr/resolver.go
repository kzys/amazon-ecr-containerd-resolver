/*
 * Copyright 2017-2020 Amazon.com, Inc. or its affiliates. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"). You
 * may not use this file except in compliance with the License. A copy of
 * the License is located at
 *
 * 	http://aws.amazon.com/apache2.0/
 *
 * or in the "license" file accompanying this file. This file is
 * distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF
 * ANY KIND, either express or implied. See the License for the specific
 * language governing permissions and limitations under the License.
 */

package ecr

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrsdk "github.com/aws/aws-sdk-go/service/ecr"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var unimplemented = errors.New("unimplemented")

type ecrResolver struct {
	session                  *session.Session
	clients                  map[string]ecrAPI
	clientsLock              sync.Mutex
	tracker                  docker.StatusTracker
	layerDownloadParallelism int
}

// ResolverOption represents a functional option for configuring the ECR
// Resolver
type ResolverOption func(*ResolverOptions) error

// ResolverOptions represents available options for configuring the ECR Resolver
type ResolverOptions struct {
	// Session is used for configuring the ECR client.  If not specified, a
	// generic session is used.
	Session *session.Session
	// Tracker is used to track uploads to ECR.  If not specified, an in-memory
	// tracker is used instead.
	Tracker docker.StatusTracker
	// LayerDownloadParallelism configures whether layer parts should be
	// downloaded in parallel.  If not specified, parallelism is currently
	// disabled.
	LayerDownloadParallelism int
}

// WithSession is a ResolverOption to use a specific AWS session.Session
func WithSession(session *session.Session) ResolverOption {
	return func(options *ResolverOptions) error {
		options.Session = session
		return nil
	}
}

// WithTracker is a ResolverOption to use a specific docker.Tracker
func WithTracker(tracker docker.StatusTracker) ResolverOption {
	return func(options *ResolverOptions) error {
		options.Tracker = tracker
		return nil
	}
}

// WithLayerDownloadParallelism is a ResolverOption to configure whether layer
// parts should be downloaded in parallel.  Layer parallelism is backed by the
// htcat library and can increase the speed at which layers are downloaded at
// the cost of increased memory consumption.  It is recommended to test your
// workload to determine whether the tradeoff is worthwhile.
func WithLayerDownloadParallelism(parallelism int) ResolverOption {
	return func(options *ResolverOptions) error {
		options.LayerDownloadParallelism = parallelism
		return nil
	}
}

// NewResolver creates a new remotes.Resolver capable of interacting with Amazon
// ECR.  NewResolver can be called with no arguments for default configuration,
// or can be customized by specifying ResolverOptions.  By default, NewResolver
// will allocate a new AWS session.Session and an in-memory tracker for layer
// progress.
func NewResolver(options ...ResolverOption) (remotes.Resolver, error) {
	resolverOptions := &ResolverOptions{}
	for _, option := range options {
		err := option(resolverOptions)
		if err != nil {
			return nil, err
		}
	}
	if resolverOptions.Session == nil {
		awsSession, err := session.NewSession()
		if err != nil {
			return nil, err
		}
		resolverOptions.Session = awsSession
	}
	if resolverOptions.Tracker == nil {
		resolverOptions.Tracker = docker.NewInMemoryTracker()
	}
	return &ecrResolver{
		session:                  resolverOptions.Session,
		clients:                  map[string]ecrAPI{},
		tracker:                  resolverOptions.Tracker,
		layerDownloadParallelism: resolverOptions.LayerDownloadParallelism,
	}, nil
}

// Resolve attempts to resolve the provided reference into a name and a
// descriptor.
//
// Valid references are of the form "ecr.aws/arn:aws:ecr:<region>:<account>:repository/<name>:<tag>".
func (r *ecrResolver) Resolve(ctx context.Context, ref string) (string, ocispec.Descriptor, error) {
	ecrSpec, err := ParseRef(ref)
	if err != nil {
		return "", ocispec.Descriptor{}, err
	}

	if ecrSpec.Object == "" {
		return "", ocispec.Descriptor{}, reference.ErrObjectRequired
	}

	batchGetImageInput := &ecr.BatchGetImageInput{
		RegistryId:         aws.String(ecrSpec.Registry()),
		RepositoryName:     aws.String(ecrSpec.Repository),
		ImageIds:           []*ecr.ImageIdentifier{ecrSpec.ImageID()},
		AcceptedMediaTypes: aws.StringSlice(supportedImageMediaTypes),
	}

	client := r.getClient(ecrSpec.Region())

	batchGetImageOutput, err := client.BatchGetImageWithContext(ctx, batchGetImageInput)
	if err != nil {
		log.G(ctx).
			WithField("ref", ref).
			WithError(err).
			Warn("Failed while calling BatchGetImage")
		return "", ocispec.Descriptor{}, err
	}
	log.G(ctx).
		WithField("ref", ref).
		WithField("batchGetImageOutput", batchGetImageOutput).
		Debug("ecr.resolver.resolve")

	if len(batchGetImageOutput.Images) == 0 {
		return "", ocispec.Descriptor{}, reference.ErrInvalid
	}
	ecrImage := batchGetImageOutput.Images[0]

	mediaType := aws.StringValue(ecrImage.ImageManifestMediaType)
	if mediaType == "" {
		manifestBody := aws.StringValue(ecrImage.ImageManifest)
		log.G(ctx).
			WithField("ref", ref).
			WithField("manifest", manifestBody).
			Trace("ecr.resolver.resolve: parsing mediaType from manifest")
		mediaType = parseImageManifestMediaType(ctx, manifestBody)
	}
	log.G(ctx).
		WithField("ref", ref).
		WithField("mediaType", mediaType).
		Debug("ecr.resolver.resolve")
	// check resolved image's mediaType, it should be one of the specified in
	// the request.
	for i, accepted := range aws.StringValueSlice(batchGetImageInput.AcceptedMediaTypes) {
		if mediaType == accepted {
			break
		}
		if i+1 == len(batchGetImageInput.AcceptedMediaTypes) {
			log.G(ctx).
				WithField("ref", ref).
				WithField("mediaType", mediaType).
				Debug("ecr.resolver.resolve: unrequested mediaType, deferring to caller")
		}
	}

	desc := ocispec.Descriptor{
		Digest:    digest.Digest(aws.StringValue(ecrImage.ImageId.ImageDigest)),
		MediaType: mediaType,
		Size:      int64(len(aws.StringValue(ecrImage.ImageManifest))),
	}
	// assert matching digest if the provided ref includes one.
	if expectedDigest := ecrSpec.Spec().Digest().String(); expectedDigest != "" &&
		desc.Digest.String() != expectedDigest {
		return "", ocispec.Descriptor{}, errors.Wrap(errdefs.ErrFailedPrecondition, "resolved image digest mismatch")
	}

	return ecrSpec.Canonical(), desc, nil
}

func (r *ecrResolver) getClient(region string) ecrAPI {
	r.clientsLock.Lock()
	defer r.clientsLock.Unlock()
	if _, ok := r.clients[region]; !ok {
		r.clients[region] = ecrsdk.New(r.session, &aws.Config{Region: aws.String(region)})
	}
	return r.clients[region]
}

// manifestProbe provides a structure to parse and then probe a given manifest
// to determine its mediaType.
type manifestProbe struct {
	// SchemaVersion is version identifier for the manifest schema used.
	SchemaVersion int64 `json:"schemaVersion"`
	// Explicit MediaType assignment for the manifest.
	MediaType string `json:"mediaType,omitempty"`
	// Docker Schema 1 signatures.
	Signatures []json.RawMessage `json:"signatures,omitempty"`
	// OCI or Docker Manifest Lists, the list of descriptors has mediaTypes
	// embedded.
	Manifests []json.RawMessage `json:"manifests,omitempty"`
}

// TODO: add error to signal unparsable and unhandled manifest types.
func parseImageManifestMediaType(ctx context.Context, body string) string {
	// The unsigned variant of Docker v2 Schema 1 manifest mediaType.
	const mediaTypeDockerSchema1ManifestUnsigned = "application/vnd.docker.distribution.manifest.v1+json"

	// The type used as a fallback when parsing is not possible.
	const unparsedMediaType = images.MediaTypeDockerSchema2Manifest

	var manifest manifestProbe
	err := json.Unmarshal([]byte(body), &manifest)
	if err != nil {
		log.G(ctx).WithField("manifest", body).
			WithError(err).Warn("ecr.resolver.resolve: could not parse manifest")
		return unparsedMediaType
	}

	switch manifest.SchemaVersion {
	case 2:
		// Defer to the manifest declared type.
		if manifest.MediaType != "" {
			return manifest.MediaType
		}
		// Is a manifest list.
		if len(manifest.Manifests) > 0 {
			return images.MediaTypeDockerSchema2ManifestList
		}
		// Is a single image manifest.
		return images.MediaTypeDockerSchema2Manifest

	case 1:
		// Defer to the manifest declared type.
		if manifest.MediaType != "" {
			return manifest.MediaType
		}
		// Is Signed Docker Schema 1 manifest.
		if len(manifest.Signatures) > 0 {
			return images.MediaTypeDockerSchema1Manifest
		}
		// Is Unsigned Docker Schema 1 manifest.
		return mediaTypeDockerSchema1ManifestUnsigned
	}

	return ""
}

func (r *ecrResolver) Fetcher(ctx context.Context, ref string) (remotes.Fetcher, error) {
	log.G(ctx).WithField("ref", ref).Debug("ecr.resolver.fetcher")
	ecrSpec, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	return &ecrFetcher{
		ecrBase: ecrBase{
			client:  r.getClient(ecrSpec.Region()),
			ecrSpec: ecrSpec,
		},
		parallelism: r.layerDownloadParallelism,
	}, nil
}

func (r *ecrResolver) Pusher(ctx context.Context, ref string) (remotes.Pusher, error) {
	log.G(ctx).WithField("ref", ref).Debug("ecr.resolver.pusher")
	ecrSpec, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	// TODO block pushing by digest since that's not allowed
	// see containerd/remotes/docker/resolver.go:218

	if ecrSpec.Object != "" && strings.Contains(ecrSpec.Object, "@") {
		return nil, errors.New("pusher: cannot use digest reference for push location")
	}

	return &ecrPusher{
		ecrBase: ecrBase{
			client:  r.getClient(ecrSpec.Region()),
			ecrSpec: ecrSpec,
		},
		tracker: r.tracker,
	}, nil
}
