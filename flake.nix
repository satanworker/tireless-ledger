{
  description = "tireless-ledger: recall CLI, raw uploader and Telegram exporter (pi-memoryd itself ships as a release binary)";

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
        tireless-telegram = pkgs.writeShellScriptBin "tireless-telegram" ''
          exec ${pkgs.python3.withPackages (ps: [ ps.telethon ])}/bin/python3 ${./cmd/tireless-telegram/tireless_telegram.py} "$@"
        '';
      });

      # Recall CLI, its stores file and the agent skill (~/.agents/skills: omp, Codex, Pi;
      # ~/.claude/skills: Claude Code). Machines without Nix get the same from `tireless init`.
      homeManagerModules.default = { config, lib, pkgs, ... }:
        let cfg = config.programs.tireless; in
        {
          options.programs.tireless = {
            enable = lib.mkEnableOption "the tireless recall CLI and its agent skill";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.tireless;
              description = "The `tireless` command the skill calls.";
            };
            stores = lib.mkOption {
              type = lib.types.attrsOf lib.types.str;
              default = { };
              example = { personal = "100.127.82.49"; valantis = "100.97.80.65"; };
              description = "Store name -> host; the server is host:8090 and its embedder host:8091.";
            };
          };
          config = lib.mkIf cfg.enable {
            home.packages = [ cfg.package ];
            xdg.configFile."tireless/stores" = lib.mkIf (cfg.stores != { }) {
              text = lib.concatStrings (lib.mapAttrsToList (name: host: "${name} http://${host}:8090 http://${host}:8091\n") cfg.stores);
            };
            home.file.".agents/skills/tireless-recall".source = ./cmd/tireless/skill;
            home.file.".claude/skills/tireless-recall".source = ./cmd/tireless/skill;
          };
        };
    };
}
