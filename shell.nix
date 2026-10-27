# Development shell for planb: `nix-shell` (or `use nix` with direnv).
{ pkgs ? import <nixpkgs> { } }:

pkgs.mkShell {
  packages = with pkgs; [
    go
    gopls
    gotools
    golangci-lint
    kubernetes-controller-tools # controller-gen: deepcopy, CRDs, RBAC
    kubectl
    kubernetes-helm
    kind
    postgresql_18 # pg_dump / pg_restore / psql for running the agent locally
    rclone # local S3 endpoint for hack/e2e-local.sh
    gnumake
  ];

  shellHook = ''
    export GOPATH="$HOME/go"
    export PATH="$PWD/bin:$GOPATH/bin:$PATH"
    export CGO_ENABLED=0
  '';
}
