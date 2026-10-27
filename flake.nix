{
  description = "planb - backup & restore operator for PostgreSQL and S3 buckets";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.1.0";
    in
    {
      # `nix develop` - same tools as shell.nix.
      devShells = forAllSystems (pkgs: {
        default = import ./shell.nix { inherit pkgs; };
      });

      # `nix build` - the planb binary.
      packages = forAllSystems (pkgs: rec {
        planb = pkgs.buildGoModule {
          pname = "planb";
          inherit version;
          src = self;
          subPackages = [ "cmd/planb" ];
          # After changing go.mod/go.sum: set to pkgs.lib.fakeHash, run `nix build`,
          # and copy the "got:" hash from the error message here.
          vendorHash = pkgs.lib.fakeHash;
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" "-X main.version=${version}" ];
          meta.mainProgram = "planb";
        };
        default = planb;
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}
