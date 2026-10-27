# Development shell for planb: `nix-shell` (or `use nix` with direnv).
{ pkgs ? import <nixpkgs> { } }:

let
  # Editor tooling built with the same Go as the shell. Linked at .direnv/go-tools so
  # VS Code can find it at a stable path (see .vscode/settings.json "go.alternateTools").
  goTools = pkgs.symlinkJoin {
    name = "planb-go-tools";
    paths = with pkgs; [ gopls delve impl gotests gomodifytags gotools ];
  };
in
pkgs.mkShell {
  packages = with pkgs; [
    go
    goTools
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
    export PATH="$PWD/bin:$PATH:$GOPATH/bin" # nix-provided tools win over ~/go/bin
    export CGO_ENABLED=0
    mkdir -p .direnv && ln -sfn ${goTools} .direnv/go-tools
  '';
}
