{ self, inputs, lib, ... }:
{
  # Provide overlay to add `nix-snapshotter`.
  flake.overlays.default = self: super: {
    nix-snapshotter = self.callPackage ../../package.nix {
      inherit (inputs) globset;
    };

    k3s = super.k3s_1_30.override {
      buildGoModule = args: super.buildGoModule (args // super.lib.optionalAttrs (args.pname != "k3s-cni-plugins" && args.pname != "k3s-containerd") {
        vendorHash = {
          "sha256-fs9p6ywS5XCeJSF5ovDG40o+H4p4QmEJ0cvU5T9hwuA=" = "sha256-htanp0VOMadzoIyPUT8kOTSb58sz5DHlVBVGbY13ejU=";
          "sha256-q/cRKuqXuzPcLEYD+BH82ZAc+ZgGIqKWLsM1E4uQsok=" = "sha256-tJBi4b3OGrGbc49+LNx0PXwj9NuvBB55DNvIG1VCMh8=";
        }.${args.vendorHash};
        # Source https://patch-diff.githubusercontent.com/raw/k3s-io/k3s/pull/9319.patch
        # Remove when merged
        patches = (args.patches or [ ]) ++ [
          ./patches/k3s-nix-snapshotter.patch
        ];
      });
    };
  };

  perSystem = { system, ... }: {
    _module.args.pkgs = import inputs.nixpkgs {
      inherit system;
      # Apply default overlay to provide nix-snapshotter for NixOS tests &
      # configurations.
      overlays = [ self.overlays.default ];
    };
  };
}
