{
  description = "nix-utils";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];

      forEachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forEachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          hydra-build-status = pkgs.buildGoModule {
            pname = "hydra-build-status";
            version = "0.2.0";
            src = ./.;
            vendorHash = null;
            env.CGO_ENABLED = 0;

            meta = {
              description = "Report last Hydra build status for given platforms and packages with zero-DoS cache pre-checking";
              homepage = "https://github.com/imincik/nix-utils";
              license = pkgs.lib.licenses.mit;
              mainProgram = "hydra-build-status";
            };
          };

          default = self.packages.${system}.hydra-build-status;
        }
      );
    };
}
