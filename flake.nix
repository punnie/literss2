{
  description = "literss: a minimalist river-of-news reader backed by Miniflux";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        literss = pkgs.callPackage ./nix/package.nix { };
        ubol = pkgs.callPackage ./nix/ubol.nix { };
        default = literss;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [ go gopls sqlite chromium ];
        };
      });

      nixosModules.default = import ./nix/module.nix self;

      checks.x86_64-linux.vm = import ./nix/test.nix self { pkgs = nixpkgs.legacyPackages.x86_64-linux; inherit (nixpkgs) lib; };
    };
}
