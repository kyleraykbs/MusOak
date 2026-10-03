# NixOS module for the musoak CLI.
#
# It installs the client for every user and drops a system-wide default
# configuration at /etc/musoak/config.json, pointed at by MUSOAK_CONFIG.
# A home-manager configuration (or the per-user XDG file) still wins, because
# musoak prefers the XDG location when it exists.
{
  config,
  lib,
  pkgs,
  defaultPackage,
}:
let
  musoakOptions = import ./musoak-options.nix { inherit lib pkgs; };

  cfg = config.programs.musoak;
in
{
  options.programs.musoak = musoakOptions // {
    package = musoakOptions.package // {
      default = defaultPackage;
      defaultText = lib.literalExpression "musoak.packages.\${system}.musoak";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    environment.etc."musoak/config.json".source = (pkgs.formats.json { }).generate "musoak-config.json" (
      musoakOptions.configJSON cfg
    );

    environment.variables.MUSOAK_CONFIG = "/etc/musoak/config.json";
  };
}
