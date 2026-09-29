# NixOS module for the prism CLI.
#
# It installs the client for every user and drops a system-wide default
# configuration at /etc/prismusic/config.json, pointed at by PRISMUSIC_CONFIG.
# A home-manager configuration (or the per-user XDG file) still wins, because
# prism prefers the XDG location when it exists.
{
  config,
  lib,
  pkgs,
  defaultPackage,
}:
let
  prismOptions = import ./prism-options.nix { inherit lib pkgs; };

  cfg = config.programs.prism;
in
{
  options.programs.prism = prismOptions // {
    package = prismOptions.package // {
      default = defaultPackage;
      defaultText = lib.literalExpression "prismusic.packages.\${system}.prism";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    environment.etc."prismusic/config.json".source = (pkgs.formats.json { }).generate "prism-config.json" (
      prismOptions.configJSON cfg
    );

    environment.variables.PRISMUSIC_CONFIG = "/etc/prismusic/config.json";
  };
}
