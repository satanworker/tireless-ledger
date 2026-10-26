{
  description = "tireless-ledger: recall CLI and raw uploader (pi-memoryd itself ships as a release binary)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-linux" "x86_64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      # One module, one vendorHash: update it whenever go.mod/go.sum change.
      goPackage = pkgs: pname: subPackage: pkgs.buildGoModule {
        inherit pname;
        version = self.shortRev or self.dirtyShortRev or "dev";
        src = self;
        vendorHash = "sha256-CrVp4HdMLsU6H5Yg1EWZBb1gEOBgWXbQPg0asl6wjVE=";
        subPackages = [ subPackage ];
        env.CGO_ENABLED = "0";
        ldflags = [ "-s" "-w" ];
        meta.mainProgram = pname;
      };
    in
    {
      packages = forAllSystems (pkgs: {
        tireless = goPackage pkgs "tireless" "cmd/tireless";
        tireless-upload = goPackage pkgs "tireless-upload" "cmd/tireless-upload";
      });
    };
}
