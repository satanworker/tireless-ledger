{
  description = "tireless-ledger: pi-memoryd recall server, recall CLI, raw uploader and Telegram exporter";

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
      # pi-memoryd links Lance (Rust) through lancedb-go: the Dockerfile's pinned commit and
      # patches, and the published liblancedb_go.a of `make lance-lib`. Linux only.
      lanceLibs = {
        x86_64-linux = { arch = "amd64"; hash = "sha256-HiYGhLZFrHSTbz/5EU+B+YWIB3iCySExaEsE7FQrqTo="; };
        aarch64-linux = { arch = "arm64"; hash = "sha256-WwLQPZtS5ThodB4D+vN0EgGpyTv7sWsgjtM2EuZcKG8="; };
      };
      piMemoryd = pkgs:
        let
          lance = lanceLibs.${pkgs.stdenv.hostPlatform.system};
          lanceLib = pkgs.fetchurl {
            url = "https://github.com/satanworker/tireless-ledger/releases/download/lance-lib-0.40.0-r1/liblancedb_go-linux-${lance.arch}.a";
            inherit (lance) hash;
          };
          lancedbGo = pkgs.applyPatches {
            name = "lancedb-go-patched";
            src = pkgs.fetchFromGitHub {
              owner = "lancedb";
              repo = "lancedb-go";
              rev = "fa14ce29c7724354f2cea630a1d3488b56bbd64b";
              hash = "sha256-Nyr0oicdAyZaXTx5b87eaA0juYAPCwz51tZq2DeeiCg=";
            };
            patches = [ ./docker/lancedb-go-session-cache.patch ./docker/lancedb-go-disk-cache.patch ];
            postPatch = "cp ${./docker/lancedb-go-disk-cache.rs} rust/src/disk_cache.rs";
          };
        in
        pkgs.buildGoModule {
          pname = "pi-memoryd";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = self;
          vendorHash = "sha256-UEETfhYmfGymSUYKtAxdqXRzZP2Ny3gBh02/WfZ8hgY=";
          subPackages = [ "." ];
          # A relative replace keeps store paths out of the vendored modules.
          postPatch = ''
            cp -r --no-preserve=mode ${lancedbGo} lancedb-go
            go mod edit -replace github.com/lancedb/lancedb-go=./lancedb-go
          '';
          env.CGO_ENABLED = "1";
          # Vendoring keeps only Go packages; the header stays in the patched source.
          env.CGO_CFLAGS = "-I${lancedbGo}/include";
          env.CGO_LDFLAGS = "${lanceLib} -lm -ldl -lpthread";
          ldflags = [ "-s" "-w" ];
          # Lance's tests need S3 or large fixtures; `go test` with CGO_ENABLED=0 covers the rest.
          doCheck = false;
          meta.mainProgram = "pi-memoryd";
          meta.platforms = builtins.attrNames lanceLibs;
        };
    in
    {
      packages = forAllSystems (pkgs: {
        pi-memoryd = piMemoryd pkgs;
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
