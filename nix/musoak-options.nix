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
  enable = lib.mkEnableOption "the musoak music client";

  package = lib.mkOption {
    type = lib.types.package;
    description = "The musoak package to install.";
  };

  serverURL = lib.mkOption {
    type = lib.types.str;
    default = "";
    example = "http://localhost:4420";
    description = ''
      Server to talk to. Empty means musoak runs its own server in-process, so a
      standalone installation needs no daemon at all.
    '';
  };

  cacheDir = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    example = "/home/alice/.cache/musoak";
    description = ''
      Where downloaded renditions and the local queue live. Null leaves it to
      musoak, which uses $XDG_CACHE_HOME/musoak.
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

  # configJSON is the configuration file musoak reads.
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
