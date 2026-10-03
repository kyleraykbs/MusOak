{
  description = "MusOak — self-hosted music streaming, cross-provider matching and listen-together";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs =
    inputs@{ self, flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];

      flake = {
        nixosModules = {
          musoakd =
            { config, lib, pkgs, ... }:
            import ./nix/musoakd.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.musoakd;
            };
          musoak =
            { config, lib, pkgs, ... }:
            import ./nix/musoak-nixos.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.musoak;
            };
          default = self.nixosModules.musoakd;
        };

        homeManagerModules = {
          musoak =
            { config, lib, pkgs, ... }:
            import ./nix/musoak-hm.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.musoak;
            };
          default = self.homeManagerModules.musoak;
        };
      };

      perSystem =
        { pkgs, lib, self', ... }:
        let
          version = "2.0.0";

          # Things every running piece of musoak needs on PATH: the python
          # helper that talks to YT Music, the downloader and its transcoders.
          providerRuntime = [
            (pkgs.python3.withPackages (ps: [ ps.ytmusicapi ]))
            pkgs.yt-dlp
            pkgs.ffmpeg
          ];
          providerPath = lib.makeBinPath providerRuntime;

          # The Go build, with the tests that ship with it.
          musoak-unwrapped = pkgs.buildGoModule {
            pname = "musoak";
            inherit version;
            src = lib.cleanSource ./.;
            vendorHash = "sha256-qAU2YSYCvMRLuU+LJxnw1WhikvmFAQr50HcbHL6og6Q=";
            subPackages = [
              "cmd/musoak"
              "cmd/musoakd"
            ];
            # The tests render audio fixtures and drive a real mpv.
            nativeCheckInputs = [
              pkgs.ffmpeg
              pkgs.mpv
            ];
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';
            meta = {
              description = "Self-hosted music streaming with cross-provider matching and listen-together";
              homepage = "https://codeberg.org/kyleraykbs/musoak";
              mainProgram = "musoak";
            };
          };

          # Each binary is wrapped with the tooling it needs, so `nix run`
          # works with no environment setup at all.
          wrapBinary =
            name: extraPath:
            pkgs.runCommand "${name}-${version}" { nativeBuildInputs = [ pkgs.makeWrapper ]; }
              ''
                mkdir -p $out/bin
                cp ${musoak-unwrapped}/bin/${name} $out/bin/${name}
                chmod +w $out/bin/${name}
                wrapProgram $out/bin/${name} \
                  --prefix PATH : ${providerPath}${lib.optionalString (extraPath != "") ":" + extraPath}
              '';

          musoak = wrapBinary "musoak" (lib.makeBinPath [ pkgs.mpv ]);
          musoakd = wrapBinary "musoakd" "";
        in
        {
          packages = {
            inherit musoak musoakd;
            default = musoak;
          };

          apps = {
            musoak = {
              type = "app";
              program = "${musoak}/bin/musoak";
              meta.description = "MusOak client";
            };
            musoakd = {
              type = "app";
              program = "${musoakd}/bin/musoakd";
              meta.description = "MusOak server";
            };
            default = self'.apps.musoak;
          };

          checks = {
            inherit musoak musoakd;
          };

          formatter = pkgs.nixfmt;

          devShells.default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
              go-tools
              golangci-lint
              delve

              gcc

              # media tooling used by the server, the CLI and the tests
              ffmpeg
              yt-dlp
              mpv

              # For fetching songs with python libs
              (python3.withPackages (
                ps: with ps; [
                  ytmusicapi
                ]
              ))
            ];

            shellHook = ''
              export GOPATH="$PWD/.go"
              export PATH="$GOPATH/bin:$PATH"
            '';
          };
        };
    };
}
