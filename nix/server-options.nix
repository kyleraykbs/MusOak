# Shared server-side option set, used by the musoakd service module and by
# the CLI module (whose standalone mode runs the very same server).
#
# Keeping one definition means the daemon and the CLI can never disagree about
# a setting's name or meaning.
{ lib }:
rec {
  listen = lib.mkOption {
    type = lib.types.str;
    default = ":4420";
    example = "127.0.0.1:4420";
    description = "Address the HTTP server listens on.";
  };

  storageDir = lib.mkOption {
    type = lib.types.nullOr lib.types.str;
    default = null;
    example = "/var/lib/musoakd";
    description = ''
      Where the database and the media directory live. Null leaves the choice
      to musoak, which uses $XDG_DATA_HOME/musoak.
    '';
  };

  requireLogin = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = ''
      Require a login for every request. When false, anonymous callers are
      guests with read and playback access and no personal data.
    '';
  };

  registrationOpen = lib.mkOption {
    type = lib.types.bool;
    default = true;
    description = "Whether new accounts may be created.";
  };

  providers = {
    ytmusic = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Search YouTube Music and download from it.";
      };
    };

    spotify = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = ''
          Search Spotify for metadata. Spotify audio is DRM-protected and can
          never be downloaded; those tracks are played through a matched
          rendition from a downloadable provider.
        '';
      };
      clientId = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Spotify application client id.";
      };
      clientSecret = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = ''
          Spotify application client secret. The generated configuration file
          is world-readable in the Nix store, so pass this through `configFile`
          from a secret manager instead.
        '';
      };
    };
  };

  defaultProviderOrder = lib.mkOption {
    type = lib.types.listOf lib.types.str;
    default = [ "ytmusic" "spotify" ];
    description = "Provider preference used when nobody has ranked anything.";
  };

  prefetchCount = lib.mkOption {
    type = lib.types.ints.unsigned;
    default = 3;
    description = "How many upcoming tracks a client keeps downloaded.";
  };

  match = {
    threshold = lib.mkOption {
      type = lib.types.float;
      default = 0.8;
      description = ''
        Score in (0, 1] above which two renditions are treated as the same
        recording. Identical ISRCs always match; durations more than five
        seconds apart never do.
      '';
    };
  };

  listenTogether = {
    skipThreshold = lib.mkOption {
      type = lib.types.float;
      default = 2.0;
      description = "Mean vote below which a track is skipped (votes are 1-5).";
    };
    minVotersForSkip = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 2;
      description = "How many members must have voted before a skip can fire.";
    };
    voterFractionForSkip = lib.mkOption {
      type = lib.types.float;
      default = 0.5;
      description = "Fraction of the room that must have voted.";
    };
  };

  extraSettings = lib.mkOption {
    type = lib.types.attrs;
    default = { };
    example = {
      client = {
        serverURL = "http://localhost:4420";
      };
    };
    description = ''
      Extra configuration merged into the generated JSON last, keyed exactly as
      the Go config expects. Unknown keys are rejected by the server, so this
      cannot be used to smuggle in silently-ignored settings.
    '';
  };

  # configJSON renders the option values as the server's configuration file.
  configJSON =
    cfg:
    lib.recursiveUpdate {
      listen = cfg.listen;
      requireLogin = cfg.requireLogin;
      registrationOpen = cfg.registrationOpen;
      providers = {
        ytmusic = {
          enabled = cfg.providers.ytmusic.enable;
        };
        spotify = {
          enabled = cfg.providers.spotify.enable;
          clientId = cfg.providers.spotify.clientId;
          clientSecret = cfg.providers.spotify.clientSecret;
        };
      };
      defaultProviderOrder = cfg.defaultProviderOrder;
      prefetchCount = cfg.prefetchCount;
      listenTogether = {
        inherit (cfg.listenTogether)
          skipThreshold
          minVotersForSkip
          voterFractionForSkip;
      };
      match = {
        inherit (cfg.match) threshold;
      };
    } (lib.optionalAttrs (cfg.storageDir != null) {
      storageDir = cfg.storageDir;
    } // cfg.extraSettings);
}
