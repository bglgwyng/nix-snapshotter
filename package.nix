{ lib
, buildGoModule
, closureInfo
, globset
, runCommand
, writeShellScriptBin
, writeText
}:

let
  nix-snapshotter = buildGoModule {
    pname = "nix-snapshotter";
    version = "0.3.0";
    src = lib.fileset.toSource {
      root = ./.;
      fileset = globset.lib.globs ./. [
        "**/*.go"
        "**/*.tar"
        "go.mod"
        "go.sum"
      ];
    };
    vendorHash = "sha256-mWMkDALQ3QvDxgw1Nf0bgWYqeOFDUYKg3UNurNJdD9I=";
    passthru = { inherit buildImage; };
  };

  # buildImage is analogous to the `docker build` command, in that it can be
  # used to build an OCI image archive that can be loaded into containerd. Note
  # Note that nix-snapshotter is a containerd plugin, so nix-snapshotter images
  # will only work with containerd.
  buildImage = args@{
    # The image name when exported. When resolvedByNix is enabled, this is
    # treated as just the package name to help identify the nix store path.
    name,
    # The image tag when exported. By default, this mutable "latest" tag.
    tag ? "latest",
    # If enabled, the OCI archive will be generated with a special image
    # reference in the format of "nix:0/nix/store/*.tar", which is resolvable
    # by nix-snapshotter if configured as the CRI image-service without a
    # Docker Registry.
    resolvedByNix ? false,
    # An image that is used as base image of this image. Any image can be used
    # as a fromImage, including non-nix images and images built with
    # pkgs.dockerTools.buildImage.
    fromImage ? null,
    # A derivation (or list of derivation) to include in the layer
    # root. The store path prefix /nix/store/hash-path is removed. The
    # store path content is then located at the image /.
    copyToRoot ? null,
    # An attribute set describing an image configuration as defined in:
    # https://github.com/opencontainers/image-spec/blob/8b9d41f48198a7d6d0a5c1a12dc2d1f7f47fc97f/specs-go/v1/config.go#L23
    config ? {},
  }:
    let
      baseName = baseNameOf name;

      configFile = writeText "config-${baseName}.json" (builtins.toJSON config);

      copyToRootList = lib.toList (args.copyToRoot or []);

      runtimeClosureInfo = closureInfo {
        rootPaths = [ configFile ] ++ copyToRootList;
      };

      copyToRootFile =
        writeText
          "copy-to-root-${baseName}.json"
          (builtins.toJSON copyToRootList);

      fromImageFlag = lib.optionalString (fromImage != null) ''--from-image "${fromImage}"'';

      image =
        let
          imageName = lib.toLower name;

          imageRef = if resolvedByNix then "nix:0${image.outPath}" else "${imageName}:${tag}";

          refFlag = lib.optionalString (!resolvedByNix) ''--ref "${imageRef}"'';

        in runCommand "nix-image-${baseName}.tar" {
          passthru = {
            inherit name tag;
            # For kubernetes pod spec.
            image = imageRef;
            copyToRegistry = copyToRegistry image;
            copyToContainerd = copyToContainerd image;
          };
        } ''
          ${nix-snapshotter}/bin/nix2container build \
            --config "${configFile}" \
            --closure "${runtimeClosureInfo}/store-paths" \
            --copy-to-root "${copyToRootFile}" \
            ${refFlag} \
            ${fromImageFlag} \
            $out
        '';

    in image;

  # Copies an OCI archive to an OCI registry.
  copyToRegistry = image: {
    imageName ? image.name,
    imageTag ? image.tag,
    plainHTTP ? false,
  }:
    let
      plainHTTPFlag = if plainHTTP then "--plain-http" else "";

    in writeShellScriptBin "copy-to-registry" ''
      ${nix-snapshotter}/bin/nix2container push \
        --ref "${imageName}:${imageTag}" \
        ${plainHTTPFlag} \
        ${image}
    '';

  # Copies an OCI archive into containerd's image store.
  copyToContainerd = image: args@{
    address ? null,
    namespace ? null,
  }:
    let
      addressFlag =
        if args?address then "--address ${address}" else "";

      namespaceFlag =
        if args?namespace then "--namespace ${namespace}" else "";

    in writeShellScriptBin "copy-to-containerd" ''
      ${nix-snapshotter}/bin/nix2container \
        ${addressFlag} \
        ${namespaceFlag} \
        load ${image}
    '';

  # Encodes a nix flake URL to an OCI-compatible image reference.
  # Example: git+ssh://git@github.com/user/repo?ref=main
  #       -> flake-git-ssh:0/git--at--github.com/user/repo--q--ref--eq--main
  encodeFlakeRef = writeShellScriptBin "encode-flake-ref" ''
    if [ $# -eq 0 ]; then
      echo "Usage: encode-flake-ref <flake-url>" >&2
      echo "" >&2
      echo "Encodes a nix flake URL to an OCI-compatible image reference." >&2
      echo "" >&2
      echo "Examples:" >&2
      echo "  encode-flake-ref 'github:user/repo'" >&2
      echo "  encode-flake-ref 'git+ssh://git@github.com/user/repo?ref=main'" >&2
      echo "  encode-flake-ref 'git+https://github.com/user/repo'" >&2
      echo "  encode-flake-ref 'tarball+https://github.com/user/repo/archive/main.tar.gz'" >&2
      exit 1
    fi

    url="$1"

    # Encode special characters first (before protocol conversion)
    encode_special() {
      echo "$1" | sed \
        -e 's/@/--at--/g' \
        -e 's/?/--q--/g' \
        -e 's/=/--eq--/g' \
        -e 's/&/--amp--/g'
    }

    # github:user/repo -> flake-github:0/user/repo
    if [[ "$url" == github:* ]]; then
      path="''${url#github:}"
      encoded=$(encode_special "$path")
      echo "flake-github:0/$encoded"
      exit 0
    fi

    # tarball+https://host/path -> flake-tarball-https:0/host/path
    if [[ "$url" == tarball+https://* ]]; then
      path="''${url#tarball+https://}"
      encoded=$(encode_special "$path")
      echo "flake-tarball-https:0/$encoded"
      exit 0
    fi

    # tarball+http://host/path -> flake-tarball-http:0/host/path
    if [[ "$url" == tarball+http://* ]]; then
      path="''${url#tarball+http://}"
      encoded=$(encode_special "$path")
      echo "flake-tarball-http:0/$encoded"
      exit 0
    fi

    # git+https://host/path -> flake-git-https:0/host/path
    if [[ "$url" == git+https://* ]]; then
      path="''${url#git+https://}"
      encoded=$(encode_special "$path")
      echo "flake-git-https:0/$encoded"
      exit 0
    fi

    # git+http://host/path -> flake-git-http:0/host/path
    if [[ "$url" == git+http://* ]]; then
      path="''${url#git+http://}"
      encoded=$(encode_special "$path")
      echo "flake-git-http:0/$encoded"
      exit 0
    fi

    # git+ssh://user@host/path -> flake-git-ssh:0/user--at--host/path
    if [[ "$url" == git+ssh://* ]]; then
      path="''${url#git+ssh://}"
      encoded=$(encode_special "$path")
      echo "flake-git-ssh:0/$encoded"
      exit 0
    fi

    echo "Error: Unknown flake URL format: $url" >&2
    echo "" >&2
    echo "Supported formats:" >&2
    echo "  github:user/repo" >&2
    echo "  tarball+https://host/path" >&2
    echo "  tarball+http://host/path" >&2
    echo "  git+https://host/path" >&2
    echo "  git+http://host/path" >&2
    echo "  git+ssh://user@host/path" >&2
    exit 1
  '';

in {
  inherit (nix-snapshotter) pname version;
  nix-snapshotter = nix-snapshotter;
  encode-flake-ref = encodeFlakeRef;
}
