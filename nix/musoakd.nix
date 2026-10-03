# NixOS service for musoakd.
#
#   services.musoakd = {
#     enable = true;
#     storageDir = "/var/lib/musoakd";
#     listen = ":4420";
#   };
{
  config,
  lib,
  pkgs,
  defaultPackage,
}:
let
  serverOptions = import ./server-options.nix { inherit lib; };

  cfg = config.services.musoakd;

  # The configuration the server actually reads: an explicit file when the
  # user has one (that is where secrets belong), the generated JSON otherwise.
  configFile =
    if cfg.configFile != null then cfg.configFile else "/etc/musoakd/config.json";

  generatedConfig = (pkgs.formats.json { }).generate "musoakd-config.json" (
    serverOptions.configJSON cfg
  );
in
{
  options.services.musoakd = {
    enable = lib.mkEnableOption "the musoak server";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPackage;
      defaultText = lib.literalExpression "musoak.packages.\${system}.musoakd";
      description = "The musoakd package to run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "musoakd";
      description = "User the service runs as.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "musoakd";
      description = "Group the service runs as.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open the listen port in the firewall.";
    };

    configFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/run/secrets/musoakd.json";
      description = ''
        Configuration file to use instead of the generated one. Use this when
        the configuration contains secrets (the Spotify client secret), since
        the generated file lives in the world-readable Nix store.
      '';
    };

    environment = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      description = "Extra environment variables for the service.";
    };

    hardening = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Apply the systemd sandboxing options.";
    };

    inherit (serverOptions)
      listen
      requireLogin
      registrationOpen
      providers
      defaultProviderOrder
      prefetchCount
      match
      listenTogether
      extraSettings
      ;
  }
  // {
    storageDir = serverOptions.storageDir // {
      default = "/var/lib/musoakd";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      home = cfg.storageDir;
      description = "musoak server";
    };
    users.groups.${cfg.group} = { };

    systemd.services.musoakd = {
      description = "MusOak server";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      environment = cfg.environment;

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/musoakd --config ${configFile}";
        User = cfg.user;
        Group = cfg.group;
        Restart = "on-failure";
        RestartSec = 2;
        StateDirectory = "musoakd";
        StateDirectoryMode = "0750";
        WorkingDirectory = cfg.storageDir;
        # The media library and the database are the only writable places, and
        # the tools the providers shell out to need a temp directory.
        ReadWritePaths = [ cfg.storageDir ];
      }
      // lib.optionalAttrs cfg.hardening {
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = false; # opus encoding uses JIT-less ffmpeg, but keep this off for safety
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
          "AF_NETLINK"
        ];
      };
    };

    # The generated configuration is written to /etc so it is obvious and
    # inspectable; a user-provided file is used exactly as it is.
    environment.etc = lib.mkIf (cfg.configFile == null) {
      "musoakd/config.json".source = generatedConfig;
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [
      (lib.toInt (lib.last (lib.splitString ":" cfg.listen)))
    ];
  };
}
