package nix2container

import (
	"context"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/pdtpartners/nix-snapshotter/types"
)

var (
	// ImageRefPrefix is part of the canonical image reference for images built
	// for nix-snapshotter in the format "nix:0/nix/store/*.tar".
	//
	// Leading slash is not allowed for image references, so we needed a distinct
	// prefix for nix-snapshotter to distinguish regular references from nix
	// references. If nix-snapshotter is configured as the CRI image service,
	// it will be able to resolve the image manifest with nix rather than a
	// Docker Registry.
	ImageRefPrefix = "nix:0"

	// FlakeGitHubRefPrefix is used for GitHub flake-based image references in the format
	// "flake-github:0/user/repo". This allows building images from GitHub flake URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "github:user/repo".
	// Example: "flake-github:0/bglgwyng/redis-image"
	FlakeGitHubRefPrefix = "flake-github:0/"

	// FlakeTarballHTTPSRefPrefix is used for tarball HTTPS flake-based image references in the format
	// "flake-tarball-https:0/host/path". This allows building images from tarball HTTPS URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "tarball+https://host/path".
	// Example: "flake-tarball-https:0/github.com/user/repo/archive/main.tar.gz"
	FlakeTarballHTTPSRefPrefix = "flake-tarball-https:0/"

	// FlakeTarballHTTPRefPrefix is used for tarball HTTP flake-based image references in the format
	// "flake-tarball-http:0/host/path". This allows building images from tarball HTTP URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "tarball+http://host/path".
	// Example: "flake-tarball-http:0/example.com/archive.tar.gz"
	FlakeTarballHTTPRefPrefix = "flake-tarball-http:0/"

	// FlakeGitHTTPSRefPrefix is used for git HTTPS flake-based image references in the format
	// "flake-git-https:0/host/path". This allows building images from git HTTPS URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "git+https://host/path".
	// Example: "flake-git-https:0/github.com/user/repo"
	FlakeGitHTTPSRefPrefix = "flake-git-https:0/"

	// FlakeGitHTTPRefPrefix is used for git HTTP flake-based image references in the format
	// "flake-git-http:0/host/path". This allows building images from git HTTP URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "git+http://host/path".
	// Example: "flake-git-http:0/example.com/user/repo"
	FlakeGitHTTPRefPrefix = "flake-git-http:0/"

	// FlakeGitSSHRefPrefix is used for git SSH flake-based image references in the format
	// "flake-git-ssh:0/host/path". This allows building images from git SSH URLs directly,
	// without requiring the nix store path to exist on the node beforehand.
	// The format is OCI-compatible (host:port/path) and gets converted to "git+ssh://host/path".
	// Note: Use "--at--" to encode "@" since "@" is reserved as digest separator in OCI refs.
	// Example: "flake-git-ssh:0/git--at--github.com/user/repo" -> "git+ssh://git@github.com/user/repo"
	FlakeGitSSHRefPrefix = "flake-git-ssh:0/"
)

// Export writes an OCI archive to the writer using the provided nix image
// spec.
func Export(ctx context.Context, store content.Store, image *types.Image, ref string, w io.Writer) error {
	desc, err := Generate(ctx, image, store)
	if err != nil {
		return err
	}

	exportOpts := []archive.ExportOpt{
		archive.WithManifest(desc, ref),
	}

	return archive.Export(ctx, store, w, exportOpts...)
}
