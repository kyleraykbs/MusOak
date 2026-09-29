{
  description = "Prismusic — self-hosted music streaming, cross-provider matching and listen-together";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];

      perSystem =
        { pkgs, ... }:
        {
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
