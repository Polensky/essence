{
  description = "Essence Quebec - Gas price heatmap";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
          ];
        };

        packages.default = pkgs.buildGoModule {
          pname = "essence";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-iiobqg0INKuzTyC/VMGcX5CfroqqbKnwlJLVAOCZbEE=";
        };
      }
    ) // {
      nixosModules.default = import ./nixos-module.nix { inherit self; };
    };
}
