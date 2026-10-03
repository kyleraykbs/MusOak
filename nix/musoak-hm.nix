# home-manager module for the musoak CLI.
#
# It installs musoak for one user and writes that user's configuration to
# $XDG_CONFIG_HOME/musoak/config.json, which musoak reads by default.
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
    home.packages = [ cfg.package ];

    xdg.configFile."musoak/config.json".source =
      (pkgs.formats.json { }).generate "musoak-config.json" (musoakOptions.configJSON cfg);
  };
}
