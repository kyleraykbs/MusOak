# Options shared by the NixOS and home-manager CLI modules.
#
# The CLI is an API client. With `serverURL` set it talks to a daemon; with it
# empty it runs the server inside the CLI process, which is why the server
# settings appear here too.
{ lib, pkgs }:
let
  serverOptions = import ./server-options.nix { inherit lib; };
in
{
  enable = lib.mkEnableOption "the prism music client";

  package = lib.mkOption {
    type = lib.types.package;
    description = "The prism package to install.";
  };

  serverURL = lib.mkOption {
    type = lib.types.str;
    default = "";
    example = "http://localhost:8080";
    description = ''
      Server to talk to. Empty means prism runs its own server in-process, so a
      standalone installation needs no daemon at all.
    '';
  };

  cacheDir = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    example = "/home/alice/.cache/prismusic";
    description = ''
      Where downloaded renditions and the local queue live. Null leaves it to
      prism, which uses $XDG_CACHE_HOME/prismusic.
    '';
  };

  server = {
    inherit (serverOptions)
      listen
      storageDir
      requireLogin
      registrationOpen
      providers
      defaultProviderOrder
      prefetchCount
      match
      listenTogether
      extraSettings
      ;
  };

  # configJSON is the configuration file prism reads.
  configJSON =
    cfg:
    lib.recursiveUpdate (serverOptions.configJSON cfg.server) {
      client = {
        serverURL = cfg.serverURL;
      }
      // lib.optionalAttrs (cfg.cacheDir != null) {
        cacheDir = cfg.cacheDir;
      };
    };
}
