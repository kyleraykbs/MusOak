{
  description = "Prismusic — self-hosted music streaming, cross-provider matching and listen-together";

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
          prismusicd =
            { config, lib, pkgs, ... }:
            import ./nix/prismusicd.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.prismusicd;
            };
          prism =
            { config, lib, pkgs, ... }:
            import ./nix/prism-nixos.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.prism;
            };
          default = self.nixosModules.prismusicd;
        };

        homeManagerModules = {
          prism =
            { config, lib, pkgs, ... }:
            import ./nix/prism-hm.nix {
              inherit config lib pkgs;
              defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.prism;
            };
          default = self.homeManagerModules.prism;
        };
      };

      perSystem =
        { pkgs, lib, self', ... }:
        let
          version = "2.0.0";

          # Things every running piece of prismusic needs on PATH: the python
          # helper that talks to YT Music, the downloader and its transcoders.
          providerRuntime = [
            (pkgs.python3.withPackages (ps: [ ps.ytmusicapi ]))
            pkgs.yt-dlp
            pkgs.ffmpeg
          ];
          providerPath = lib.makeBinPath providerRuntime;

          # The Go build, with the tests that ship with it.
          prismusic-unwrapped = pkgs.buildGoModule {
            pname = "prismusic";
            inherit version;
            src = lib.cleanSource ./.;
            vendorHash = "sha256-YRe55i9oXFG7mibRc4In8rXUJsFA+N0KNL/7HheCvbU=";
            subPackages = [
              "cmd/prism"
              "cmd/prismusicd"
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
              homepage = "https://codeberg.org/kyleraykbs/prismusic";
              license = lib.licenses.mit;
              mainProgram = "prism";
            };
          };

          # Each binary is wrapped with the tooling it needs, so `nix run`
          # works with no environment setup at all.
          wrapBinary =
            name: extraPath:
            pkgs.runCommand "${name}-${version}" { nativeBuildInputs = [ pkgs.makeWrapper ]; }
              ''
                mkdir -p $out/bin
                cp ${prismusic-unwrapped}/bin/${name} $out/bin/${name}
                chmod +w $out/bin/${name}
                wrapProgram $out/bin/${name} \
                  --prefix PATH : ${providerPath}${lib.optionalString (extraPath != "") ":" + extraPath}
              '';

          prism = wrapBinary "prism" (lib.makeBinPath [ pkgs.mpv ]);
          prismusicd = wrapBinary "prismusicd" "";
        in
        {
          packages = {
            inherit prism prismusicd;
            default = prism;
          };

          apps = {
            prism = {
              type = "app";
              program = "${prism}/bin/prism";
              meta.description = "Prismusic client";
            };
            prismusicd = {
              type = "app";
              program = "${prismusicd}/bin/prismusicd";
              meta.description = "Prismusic server";
            };
            default = self'.apps.prism;
          };

          checks = {
            inherit prism prismusicd;
          };

          formatter = pkgs.nixfmt-rfc-style;

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
