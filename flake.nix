{
  description = "spiffec: per-connection stdio to SPIFFE mTLS client tunnel";

  inputs.nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      lib = nixpkgs.lib;
      systems = [ "x86_64-linux" ];
      forAllSystems =
        f:
        lib.genAttrs systems (
          system:
          f {
            inherit system;
            pkgs = nixpkgs.legacyPackages.${system};
          }
        );
    in
    {
      packages = forAllSystems (
        { pkgs, ... }:
        let
          package = pkgs.callPackage ./package.nix { };
        in
        {
          default = package;
          spiffec = package;
        }
      );

      overlays.default = final: _prev: {
        spiffec = final.callPackage ./package.nix { };
      };

      devShells = forAllSystems (
        { pkgs, ... }:
        {
          default = pkgs.mkShell {
            packages = [ pkgs.go ];
          };
        }
      );

      checks = forAllSystems (
        { pkgs, system }:
        {
          spiffec = self.packages.${system}.spiffec;
        }
      );
    };
}
