# home-manager module for the prism CLI.
#
# It installs prism for one user and writes that user's configuration to
# $XDG_CONFIG_HOME/prismusic/config.json, which prism reads by default.
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
    home.packages = [ cfg.package ];

    xdg.configFile."prismusic/config.json".source =
      (pkgs.formats.json { }).generate "prism-config.json" (prismOptions.configJSON cfg);
  };
}
