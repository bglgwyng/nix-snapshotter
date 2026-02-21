package nix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/pdtpartners/nix-snapshotter/pkg/nix2container"
	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var (
	ErrNotInitialized = errors.New("Nix-snapshotter Image Service not yet initialized")
)

// ImageServiceConfig is used to configure the image service instance.
type ImageServiceConfig struct {
	Config
}

// ImageServiceOpt is an option for NewImageService.
type ImageServiceOpt interface {
	SetImageServiceOpt(cfg *ImageServiceConfig)
}

type imageService struct {
	mu                 sync.Mutex
	client             *client.Client
	imageServiceClient runtime.ImageServiceClient
	nixBuilder         NixBuilder
	flakeBuilder       FlakeBuilder
}

func NewImageService(ctx context.Context, containerdAddr string, opts ...ImageServiceOpt) (runtime.ImageServiceServer, error) {
	cfg := ImageServiceConfig{
		Config: Config{
			nixBuilder:   defaultNixBuilder,
			flakeBuilder: defaultFlakeBuilder,
		},
	}
	for _, opt := range opts {
		opt.SetImageServiceOpt(&cfg)
	}

	service := &imageService{
		nixBuilder:   cfg.nixBuilder,
		flakeBuilder: cfg.flakeBuilder,
	}

	go func() {
		log.G(ctx).Debugf("Waiting for CRI service is started...")
		for i := 0; i < 100; i++ {
			client, err := client.New(containerdAddr)
			if err == nil {
				service.mu.Lock()
				service.client = client
				service.imageServiceClient = runtime.NewImageServiceClient(client.Conn().(*grpc.ClientConn))
				service.mu.Unlock()
				log.G(ctx).Info("Connected to backend CRI service")
				return
			}
			log.G(ctx).WithError(err).Warnf("Failed to connect to CRI")
			time.Sleep(10 * time.Second)
		}
		log.G(ctx).Warnf("No connection is available to CRI")
	}()

	return service, nil
}

func (is *imageService) getClient() runtime.ImageServiceClient {
	is.mu.Lock()
	client := is.imageServiceClient
	is.mu.Unlock()
	return client
}

// ListImages lists existing images.
func (is *imageService) ListImages(ctx context.Context, req *runtime.ListImagesRequest) (*runtime.ListImagesResponse, error) {
	client := is.getClient()
	if client == nil {
		return nil, ErrNotInitialized
	}
	return client.ListImages(ctx, req)
}

// ImageStatus returns the status of the image. If the image is not
// present, returns a response with ImageStatusResponse.Image set to
// nil.
func (is *imageService) ImageStatus(ctx context.Context, req *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	client := is.getClient()
	if client == nil {
		return nil, ErrNotInitialized
	}
	return client.ImageStatus(ctx, req)
}

// PullImage pulls an image with authentication config.
func (is *imageService) PullImage(ctx context.Context, req *runtime.PullImageRequest) (*runtime.PullImageResponse, error) {
	client := is.getClient()
	if client == nil {
		return nil, ErrNotInitialized
	}

	ref := req.Image.Image

	log.G(ctx).WithField("ref", ref).WithField("flakeGitHubPrefix", nix2container.FlakeGitHubRefPrefix).Info("[image-service] PullImage called")

	// Handle flake-github:0/ prefix
	// Converts "flake-github:0/user/repo" to "github:user/repo" for nix build
	if strings.HasPrefix(ref, nix2container.FlakeGitHubRefPrefix) {
		// Extract user/repo part and convert to github:user/repo format
		repoPath := strings.TrimPrefix(ref, nix2container.FlakeGitHubRefPrefix)
		// Remove :latest or other tags that k8s might append (not valid for flake refs)
		repoPath = strings.TrimSuffix(repoPath, ":latest")
		flakeRef := "github:" + repoPath
		log.G(ctx).WithField("flakeRef", flakeRef).Info("[image-service] Building flake image from GitHub")

		archivePath, err := is.flakeBuilder(ctx, flakeRef)
		if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Handle flake-tarball-https:0/ prefix
	// Converts "flake-tarball-https:0/host/path" to "tarball+https://host/path" for nix build
	if strings.HasPrefix(ref, nix2container.FlakeTarballHTTPSRefPrefix) {
		// Extract host/path part and convert to tarball+https://host/path format
		urlPath := strings.TrimPrefix(ref, nix2container.FlakeTarballHTTPSRefPrefix)
		// Remove :latest or other tags that k8s might append (not valid for flake refs)
		urlPath = strings.TrimSuffix(urlPath, ":latest")
		flakeRef := "tarball+https://" + urlPath
		log.G(ctx).WithField("flakeRef", flakeRef).Info("[image-service] Building flake image from tarball HTTPS")

		archivePath, err := is.flakeBuilder(ctx, flakeRef)
		if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Handle flake-tarball-http:0/ prefix
	// Converts "flake-tarball-http:0/host/path" to "tarball+http://host/path" for nix build
	if strings.HasPrefix(ref, nix2container.FlakeTarballHTTPRefPrefix) {
		// Extract host/path part and convert to tarball+http://host/path format
		urlPath := strings.TrimPrefix(ref, nix2container.FlakeTarballHTTPRefPrefix)
		// Remove :latest or other tags that k8s might append (not valid for flake refs)
		urlPath = strings.TrimSuffix(urlPath, ":latest")
		flakeRef := "tarball+http://" + urlPath
		log.G(ctx).WithField("flakeRef", flakeRef).Info("[image-service] Building flake image from tarball HTTP")

		archivePath, err := is.flakeBuilder(ctx, flakeRef)
		if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Handle flake-git-https:0/ prefix
	// Converts "flake-git-https:0/host/path" to "git+https://host/path" for nix build
	if strings.HasPrefix(ref, nix2container.FlakeGitHTTPSRefPrefix) {
		// Extract host/path part and convert to git+https://host/path format
		urlPath := strings.TrimPrefix(ref, nix2container.FlakeGitHTTPSRefPrefix)
		// Remove :latest or other tags that k8s might append (not valid for flake refs)
		urlPath = strings.TrimSuffix(urlPath, ":latest")
		flakeRef := "git+https://" + urlPath
		log.G(ctx).WithField("flakeRef", flakeRef).Info("[image-service] Building flake image from git HTTPS")

		archivePath, err := is.flakeBuilder(ctx, flakeRef)
		if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Handle flake-git-ssh:0/ prefix
	// Converts "flake-git-ssh:0/host/path" to "git+ssh://host/path" for nix build
	// Note: "--at--" is used to encode "@" since "@" is reserved as digest separator in OCI refs
	if strings.HasPrefix(ref, nix2container.FlakeGitSSHRefPrefix) {
		// Extract host/path part and convert to git+ssh://host/path format
		urlPath := strings.TrimPrefix(ref, nix2container.FlakeGitSSHRefPrefix)
		// Remove :latest or other tags that k8s might append (not valid for flake refs)
		urlPath = strings.TrimSuffix(urlPath, ":latest")
		// Decode "--at--" back to "@" (first occurrence only)
		// e.g., "git--at--github.com/user/repo" -> "git@github.com/user/repo"
		if !strings.Contains(urlPath, "--at--") {
			if strings.Contains(urlPath, "@") {
				return nil, fmt.Errorf("invalid flake-git-ssh reference: '@' is not allowed, use '--at--' instead (e.g., git--at--github.com) in %q", ref)
			}
			return nil, fmt.Errorf("invalid flake-git-ssh reference: missing '--at--' (encodes '@' for user@host) in %q", ref)
		}
		urlPath = strings.Replace(urlPath, "--at--", "@", 1)
		flakeRef := "git+ssh://" + urlPath
		log.G(ctx).WithField("flakeRef", flakeRef).Info("[image-service] Building flake image from git SSH")

		archivePath, err := is.flakeBuilder(ctx, flakeRef)
		if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Handle nix:0 prefix
	if strings.HasPrefix(ref, nix2container.ImageRefPrefix) {
		archivePath := strings.TrimSuffix(
			strings.TrimPrefix(ref, nix2container.ImageRefPrefix),
			":latest",
		)

		_, err := os.Stat(archivePath)
		if errors.Is(err, os.ErrNotExist) {
			log.G(ctx).Info("[image-service] Pulling nix image archive")
			err := is.nixBuilder(ctx, "", archivePath)
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}

		return is.loadArchive(ctx, archivePath)
	}

	// Fallback to CRI pull image
	log.G(ctx).WithField("ref", ref).Info("[image-service] Falling back to CRI pull image")
	resp, err := client.PullImage(ctx, req)
	return resp, err
}

// loadArchive loads an OCI archive into containerd and returns the image reference.
func (is *imageService) loadArchive(ctx context.Context, archivePath string) (*runtime.PullImageResponse, error) {
	log.G(ctx).WithField("archivePath", archivePath).Info("[image-service] Loading nix image archive")
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	img, err := nix2container.Load(ctx, is.client, archivePath)
	if err != nil {
		return nil, err
	}

	configDesc, err := img.Config(ctx)
	if err != nil {
		return nil, err
	}
	imageRef := configDesc.Digest.String()

	log.G(ctx).WithField("imageRef", imageRef).Info("[image-service] Successfully pulled image")
	return &runtime.PullImageResponse{
		ImageRef: imageRef,
	}, nil
}

// RemoveImage removes the image.
// This call is idempotent, and must not return an error if the image has
// already been removed.
func (is *imageService) RemoveImage(ctx context.Context, req *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	client := is.getClient()
	if client == nil {
		return nil, ErrNotInitialized
	}
	return client.RemoveImage(ctx, req)
}

// ImageFSInfo returns information of the filesystem that is used to store images.
func (is *imageService) ImageFsInfo(ctx context.Context, req *runtime.ImageFsInfoRequest) (*runtime.ImageFsInfoResponse, error) {
	client := is.getClient()
	if client == nil {
		return nil, ErrNotInitialized
	}
	return client.ImageFsInfo(ctx, req)
}
